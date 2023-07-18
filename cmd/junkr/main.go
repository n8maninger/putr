package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	proto2 "github.com/n8maninger/junkr/internal/rhp/v2"
	proto3 "github.com/n8maninger/junkr/internal/rhp/v3"
	"github.com/n8maninger/junkr/internal/threadgroup"
	"github.com/siacentral/apisdkgo"
	"github.com/siacentral/apisdkgo/sia"
	rhp2 "go.sia.tech/core/rhp/v2"
	"go.sia.tech/core/types"
	"go.sia.tech/renterd/api"
	"go.sia.tech/renterd/bus"
	"go.sia.tech/renterd/hostdb"
	"go.sia.tech/renterd/wallet"
	"go.sia.tech/renterd/worker"
	"go.uber.org/zap"
	"golang.org/x/crypto/blake2b"
	"lukechampine.com/frand"
)

type contractWork struct {
	contractID types.FileContractID
	hostKey    types.PublicKey
	hostAddr   string

	lockID uint64
}

var (
	masterSeed [32]byte

	workerAddr = "localhost:9980/api/worker"
	workerPass = ""

	busAddr = "localhost:9980/api/bus"
	busPass = ""

	contractSet = "autopilot"

	workers int = 15
	sectors int = 256

	logLevel string

	tg = threadgroup.New()

	start         = time.Now()
	mu            sync.Mutex // protects totalUploaded and totalCost
	totalUploaded uint64
	totalCost     types.Currency
)

func init() {
	var err error
	masterKey, err := wallet.KeyFromPhrase(os.Getenv("JUNKR_SEED"))
	if err != nil {
		panic(err)
	}
	masterSeed = blake2b.Sum256(append([]byte("worker"), masterKey...))

	flag.StringVar(&logLevel, "log.level", "info", "the log level to use")

	flag.StringVar(&workerAddr, "worker.addr", workerAddr, "the address of the renterd worker API")
	flag.StringVar(&workerPass, "worker.pass", workerPass, "the password of the renterd worker API")

	flag.StringVar(&busAddr, "bus.addr", busAddr, "the address of the renterd bus API")
	flag.StringVar(&busPass, "bus.pass", busPass, "the password of the renterd bus API")
	flag.StringVar(&contractSet, "bus.contractset", contractSet, "the contract set to use")

	flag.IntVar(&workers, "workers", workers, "the number of workers to use")
	flag.IntVar(&sectors, "sectors", sectors, "the number of sectors to upload")
	flag.Parse()
}

func formatBpsString(b uint64, t time.Duration) string {
	const units = "KMGTPE"
	const factor = 1000

	time := t.Truncate(time.Second).Seconds()
	if time <= 0 {
		return "0.00 bps"
	}

	// calculate bps
	speed := float64(b*8) / time

	// short-circuit for < 1000 bits/s
	if speed < factor {
		return fmt.Sprintf("%.2f bps", speed)
	}

	var i = -1
	for ; speed >= factor; i++ {
		speed /= factor
	}
	return fmt.Sprintf("%.2f %cbps", speed, units[i])
}

// deriveSubKey can be used to derive a sub-masterkey from the worker's
// masterkey to use for a specific purpose. Such as deriving more keys for
// ephemeral accounts.
func deriveSubKey(purpose string) types.PrivateKey {
	seed := blake2b.Sum256(append(masterSeed[:], []byte(purpose)...))
	pk := types.NewPrivateKeyFromSeed(seed[:])
	for i := range seed {
		seed[i] = 0
	}
	return pk
}

// TODO: deriving the renter key from the host key using the master key only
// works if we persist a hash of the renter's master key in the database and
// compare it on startup, otherwise there's no way of knowing the derived key is
// usuable
// NOTE: Instead of hashing the masterkey and comparing, we could use random
// bytes + the HMAC thereof as the salt. e.g. 32 bytes + 32 bytes HMAC. Then
// whenever we read a specific salt we can verify that is was created with a
// given key. That would eventually allow different masterkeys to coexist in the
// same bus.
//
// TODO: instead of deriving a renter key use a randomly generated salt so we're
// not limited to one key per host
func deriveRenterKey(hostKey types.PublicKey) types.PrivateKey {
	seed := blake2b.Sum256(append(deriveSubKey("renterkey"), hostKey[:]...))
	pk := types.NewPrivateKeyFromSeed(seed[:])
	for i := range seed {
		seed[i] = 0
	}
	return pk
}

func getContracts(ctx context.Context, bc *bus.Client) ([]api.ContractMetadata, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	return bc.ContractSetContracts(ctx, contractSet)
}

func lockContract(ctx context.Context, bc *bus.Client, contractID types.FileContractID) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	return bc.AcquireContract(ctx, contractID, 1, 15*time.Minute)
}

func releaseContract(ctx context.Context, bc *bus.Client, contractID types.FileContractID, lockID uint64) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	return bc.ReleaseContract(ctx, contractID, lockID)
}

func uploadToHost(ctx context.Context, work contractWork, log *zap.Logger) (types.Currency, error) {
	busClient := bus.NewClient(busAddr, busPass)
	workerClient := worker.NewClient(workerAddr, workerPass)

	contractID := work.contractID
	hostKey := work.hostKey
	hostAddr := work.hostAddr
	lockID := work.lockID

	defer func() {
		if err := releaseContract(context.Background(), busClient, contractID, lockID); err != nil {
			log.Warn("failed to release contract", zap.Error(err))
		}
	}()

	log = log.With(zap.Stringer("host", hostKey), zap.Stringer("contract", contractID))

	accountID, err := workerClient.Account(ctx, hostKey)
	if err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to get account: %w", err)
	}

	log.Info("uploading to host")

	settings, err := proto2.ScanSettings(ctx, hostKey, hostAddr)
	if err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to scan settings: %w", err)
	}

	addr, _, err := net.SplitHostPort(hostAddr)
	if err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to split host port: %w", err)
	}
	rhp3Addr := fmt.Sprintf("%s:%s", addr, settings.SiaMuxPort)

	session, err := proto3.NewSession(ctx, hostKey, rhp3Addr)
	if err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	pt, err := session.ScanPriceTable()
	if err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to scan price table: %w", err)
	}

	revision, err := session.Revision(contractID)
	if err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to get revision: %w", err)
	}
	contract := rhp2.ContractRevision{
		Revision: revision,
	}
	renterKey := deriveRenterKey(hostKey)
	contractPayment := proto3.ContractPayment(&contract, renterKey, accountID)

	// register a price table
	pt, err = session.RegisterPriceTable(contractPayment, pt.HostBlockHeight)
	if err != nil {
		return types.ZeroCurrency, fmt.Errorf("failed to register price table: %w", err)
	}

	// calculate the cost to upload the sectors
	uploadCost := pt.AppendSectorCost(contract.Revision.WindowEnd - pt.HostBlockHeight).Add(pt.BaseCost())
	budget, _ := uploadCost.Total()

	// add 5% to the budget to account for inconsistencies
	budget, overflow := budget.Mul64WithOverflow(21)
	if overflow {
		return types.ZeroCurrency, errors.New("cost overflow")
	}
	budget = budget.Div64(20)

	var cost types.Currency
	var interactions []hostdb.Interaction
	defer func() {
		err := busClient.RecordInteractions(context.Background(), interactions)
		if err != nil {
			log.Error("failed to record interactions", zap.Error(err))
		}
		err = busClient.RecordContractSpending(context.Background(), []api.ContractSpendingRecord{
			{
				ContractSpending: api.ContractSpending{
					Uploads: cost,
				},
				ContractID:     contractID,
				RevisionNumber: contract.Revision.RevisionNumber,
				Size:           contract.Revision.Filesize,
			},
		})
		if err != nil {
			log.Error("failed to record contract spending", zap.Error(err))
		}
	}()

	for i := 0; i < sectors; i++ {
		select {
		case <-ctx.Done():
			return cost, ctx.Err()
		default:
		}

		var sector [rhp2.SectorSize]byte
		frand.Read(sector[:])
		start := time.Now()
		sectorCost, err := session.AppendSector(&sector, &contract, renterKey, contractPayment, budget, pt.HostBlockHeight)
		interactions = append(interactions, hostdb.Interaction{
			Host:      hostKey,
			Success:   err == nil,
			Type:      "junkUpload",
			Timestamp: time.Now(),
		})
		if err != nil {
			return cost, fmt.Errorf("failed to append sector %d: %w", i, err)
		}

		cost = cost.Add(sectorCost)
		log.Debug("sector uploaded", zap.Stringer("cost", cost), zap.Duration("elapsed", time.Since(start)))
		mu.Lock()
		totalUploaded += rhp2.SectorSize
		totalCost = totalCost.Add(cost)
		mu.Unlock()
	}

	return cost, nil
}

func uploadWorker(ctx context.Context, worker int, workCh <-chan contractWork, log *zap.Logger) {
	ctx, cancel, err := tg.AddContext(ctx)
	if err != nil {
		log.Panic("failed to add thread", zap.Error(err))
	}
	defer cancel()

	log = log.With(zap.Int("worker", worker))
	for {
		select {
		case <-ctx.Done():
			return
		case work := <-workCh:
			start := time.Now()
			cost, err := uploadToHost(ctx, work, log)
			if err != nil {
				log.Error("failed to upload to host", zap.Error(err), zap.Stringer("host", work.hostKey), zap.Stringer("contract", work.contractID))
				continue
			}
			log.Info("upload complete", zap.Int("sectors", sectors), zap.Stringer("cost", cost), zap.Duration("elapsed", time.Since(start)))
		}
	}
}

func updateAllowList(busClient *bus.Client) (added, removed int, _ error) {
	siacentralClient := apisdkgo.NewSiaClient()

	// get the current allowlist
	allowlist, err := busClient.HostAllowlist(context.Background())
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get allowlist: %w", err)
	}
	// convert the allowlist to a map
	allowedHosts := make(map[types.PublicKey]bool)
	for _, pk := range allowlist {
		allowedHosts[pk] = true
	}
	// get the top 200 hosts by upload speed
	hosts, err := siacentralClient.GetActiveHosts(0, 200, sia.HostFilterBenchmarked(true), sia.HostFilterAcceptingContracts(true), sia.HostFilterSort(sia.HostSortUploadSpeed, true))
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get decent hosts: %w", err)
	}

	var toAdd, toRemove []types.PublicKey
	// add any hosts that aren't already in the allowlist
	for _, host := range hosts {
		var pk types.PublicKey
		if err := pk.UnmarshalText([]byte(host.PublicKey)); err != nil {
			continue
		}
		if allowedHosts[pk] {
			continue
		}
		toAdd = append(toAdd, pk)
	}

	// remove any hosts that are in the allowlist but aren't in the top 200
	for _, pk := range allowlist {
		if !allowedHosts[pk] {
			toRemove = append(toRemove, pk)
		}
		delete(allowedHosts, pk)
	}

	// if nothing changed, return
	if len(toAdd) == 0 && len(toRemove) == 0 {
		return 0, 0, nil
	}
	// update the allowlist
	return len(toAdd), len(toRemove), busClient.UpdateHostAllowlist(context.Background(), toAdd, toRemove, false)
}

func main() {
	cfg := zap.NewProductionConfig()
	switch logLevel {
	case "debug":
		cfg.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	case "info":
		cfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	case "warn":
		cfg.Level = zap.NewAtomicLevelAt(zap.WarnLevel)
	default:
		cfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	}
	cfg.OutputPaths = []string{"stdout"}

	log, err := cfg.Build()
	if err != nil {
		panic(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-ctx.Done()
		log.Warn("shutting down")
		tg.Stop()
		time.Sleep(time.Minute)
		os.Exit(-1)
	}()

	workCh := make(chan contractWork, workers)

	for i := 0; i < workers; i++ {
		go uploadWorker(ctx, i, workCh, log)
	}

	busClient := bus.NewClient(busAddr, busPass)

	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()

		added, removed, err := updateAllowList(busClient)
		if err != nil {
			log.Warn("failed to update allowlist", zap.Error(err))
		} else {
			log.Info("updated allowlist", zap.Int("added", added), zap.Int("removed", removed))
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			added, removed, err := updateAllowList(busClient)
			if err != nil {
				log.Warn("failed to update allowlist", zap.Error(err))
				continue
			}
			log.Info("updated allowlist", zap.Int("added", added), zap.Int("removed", removed))
		}
	}()

	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				contracts, err := getContracts(ctx, busClient)
				if err != nil {
					log.Error("failed to get contracts", zap.Error(err))
				}
				var totalSize uint64
				for _, contract := range contracts {
					totalSize += contract.Size
				}
				mu.Lock()
				d := time.Since(start)
				log.Info("upload status", zap.Int("contracts", len(contracts)), zap.Uint64("contractSize", totalSize), zap.Uint64("uploaded", totalUploaded), zap.Stringer("cost", totalCost), zap.Duration("elapsed", d), zap.String("rate", formatBpsString(totalUploaded, d)))
				mu.Unlock()
			}
		}
	}()

top:
	for {
		select {
		case <-ctx.Done():
			break top
		default:
		}

		contracts, err := getContracts(ctx, busClient)
		if err != nil {
			log.Error("failed to get contracts", zap.Error(err))
			time.Sleep(15 * time.Second)
			continue
		}

		if len(contracts) == 0 {
			log.Info("waiting for contracts")
			time.Sleep(10 * time.Minute)
			continue
		}

		log.Debug("got contracts", zap.Int("count", len(contracts)))

		for _, contract := range contracts {
			lockID, err := lockContract(ctx, busClient, contract.ID)
			if err != nil {
				log.Debug("failed to lock contract", zap.Stringer("contract", contract.ID), zap.Error(err))
				continue
			}

			log.Debug("locked contract", zap.Stringer("contract", contract.ID))
			workCh <- contractWork{
				contractID: contract.ID,
				hostKey:    contract.HostKey,
				hostAddr:   contract.HostIP,
				lockID:     lockID,
			}
		}
	}

	<-tg.Done()
}
