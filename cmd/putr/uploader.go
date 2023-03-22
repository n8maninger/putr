package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/n8maninger/putr/renter"
	"github.com/siacentral/apisdkgo"
	"github.com/siacentral/apisdkgo/sia"
	"go.sia.tech/renterd/rhp/v2"
	"go.sia.tech/siad/types"
	"lukechampine.com/frand"
)

const (
	uploadAmount = 100 << 30
	duration     = 144 * 30 * 3
)

type (
	uploadResult struct {
		Worker  int
		HostKey rhp.PublicKey
		Bytes   uint64
		Cost    types.Currency
		Err     error
	}

	host struct {
		PublicKey rhp.PublicKey
		Address   string
	}
)

func getHosts() ([]host, error) {
	sc := apisdkgo.NewSiaClient()
	filter := make(sia.HostFilter)
	filter.WithBenchmarked(true)
	filter.WithMinAge(4320)
	filter.WithMaxStoragePrice(types.SiacoinPrecision.Mul64(5000))
	filter.WithMaxUploadPrice(types.SiacoinPrecision.Mul64(500))
	filter.WithMaxContractPrice(types.SiacoinPrecision.Div64(2))
	filter.WithMinUploadSpeed(5e6) // 5 Mbps
	filter.WithSort(sia.HostSortUploadSpeed, true)

	var goodHosts []host
	for i := 0; i < 10; i++ {
		hosts, err := sc.GetActiveHosts(filter, i, 500)
		if err != nil {
			return nil, fmt.Errorf("error getting hosts: %w", err)
		} else if len(hosts) == 0 {
			break
		}

		for _, h := range hosts {
			var pub rhp.PublicKey
			if _, err := hex.Decode(pub[:], []byte(h.PublicKey[8:])); err != nil {
				return nil, fmt.Errorf("error decoding host public key: %w", err)
			}
			goodHosts = append(goodHosts, host{
				PublicKey: pub,
				Address:   h.NetAddress,
			})
		}
	}
	frand.Shuffle(len(goodHosts), func(i, j int) {
		goodHosts[i], goodHosts[j] = goodHosts[j], goodHosts[i]
	})
	return goodHosts, nil
}

func uploadData(ctx context.Context, r *renter.Renter, w renter.Wallet, hostPub rhp.PublicKey, address string, uploadFn func(uint64, types.Currency)) error {
	contract, release, err := r.HostContract(hostPub)
	if errors.Is(err, renter.ErrNoContract) {
		// if the renter doesn't have a contract with the host, form one.
		contract, release, err = r.FormContract(hostPub, address, 0, uploadAmount, duration, w)
		if err != nil {
			return fmt.Errorf("failed to form contract: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to get host contract: %w", err)
	}
	defer release()

	sessionCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	sess, err := r.NewSession(sessionCtx, hostPub, address, contract.ID)
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	go func() {
		<-ctx.Done()
		sess.Close()
	}()
	defer sess.Close()

	// get the host's current settings
	settings, err := rhp.RPCSettings(ctx, sess.Transport())
	if err != nil {
		return fmt.Errorf("failed to get settings: %w", err)
	}

	revision := sess.Revision()
	endHeight := uint64(revision.Revision.NewWindowEnd)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if r.Height() > uint64(revision.Revision.NewWindowStart)-144 {
			return r.RemoveHostContract(hostPub)
		}
		remainingDuration := endHeight - r.Height()

		var sector [1 << 22]byte
		frand.Read(sector[:])

		// calculate the cost of uploading a sector
		cost, collateral := rhp.RPCAppendCost(settings, remainingDuration)
		_, err := sess.Append(ctx, &sector, cost, collateral)
		if errors.Is(err, rhp.ErrContractFinalized) || errors.Is(err, rhp.ErrInsufficientFunds) || errors.Is(err, rhp.ErrInsufficientCollateral) {
			log.Printf("dropping contract with host %v due to error: %v", hostPub, err)
			return r.RemoveHostContract(hostPub)
		} else if err != nil && strings.Contains(err.Error(), "bad file size") { // not sure why this happens sometimes
			log.Printf("dropping contract with host %v due to error: %v", hostPub, err)
			return r.RemoveHostContract(hostPub)
		} else if err != nil {
			return fmt.Errorf("failed to append sector: %w", err)
		}
		uploadFn(1<<22, cost)
	}
}

func uploadWorker(ctx context.Context, id int, renter *renter.Renter, wallet renter.Wallet, workCh <-chan host, resultsCh chan<- uploadResult) {
	for host := range workCh {
		select {
		case <-ctx.Done():
			return
		default:
		}
		err := uploadData(ctx, renter, wallet, host.PublicKey, host.Address, func(bytes uint64, cost types.Currency) {
			resultsCh <- uploadResult{
				Worker:  id,
				HostKey: host.PublicKey,
				Bytes:   bytes,
				Cost:    cost,
			}
		})
		if err != nil {
			resultsCh <- uploadResult{
				Worker:  id,
				HostKey: host.PublicKey,
				Err:     err,
			}
			continue
		}
		time.Sleep(time.Minute)
	}
}
