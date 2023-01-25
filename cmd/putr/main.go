package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"github.com/n8maninger/putr/renter"
	"github.com/n8maninger/putr/wallet"
	"github.com/rodaine/table"
	"github.com/spf13/cobra"
	"go.sia.tech/siad/types"
	"golang.org/x/time/rate"
)

type (
	uploadStats struct {
		Bytes uint64
		Cost  types.Currency

		Hosts map[string]uint64
	}
)

var (
	dataDir string
	workers int

	mu            sync.Mutex
	totalUploaded uint64
	cost          types.Currency
	hostUploaded  = make(map[string]uint64)
	statsSync     = rate.Sometimes{First: 1, Interval: time.Minute}
)

var (
	rootCmd = &cobra.Command{
		Use:   "putr",
		Short: "putr is a command line tool for uploading files to the Sia network",
		Run: func(cmd *cobra.Command, args []string) {
			workCh := make(chan host, workers)
			resultsCh := make(chan uploadResult, 1)

			mustLoadStats()

			wallet := mustLoadWallet()
			renter, err := renter.New(dataDir)
			if err != nil {
				log.Fatalln(err)
			}
			defer renter.Close()

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()

			var wg sync.WaitGroup
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func(i int) {
					uploadWorker(ctx, i, renter, wallet, workCh, resultsCh)
					log.Printf("worker %v done", i)
					wg.Done()
				}(i)
			}

			go func() {
				var currentUploaded uint64
				s := rate.Sometimes{First: 3, Interval: time.Minute}
				// print progress
				start := time.Now()
				for result := range resultsCh {
					mu.Lock()
					totalUploaded += result.Bytes
					currentUploaded += result.Bytes
					hostUploaded[result.HostKey.String()] += result.Bytes
					cost = cost.Add(result.Cost)
					mu.Unlock()
					if result.Err != nil {
						log.Printf("worker %v error with host %v: %v", result.Worker, result.HostKey, result.Err)
					}
					s.Do(func() {
						_ = syncStats()
						printProgress(wallet, totalUploaded, currentUploaded, cost, time.Since(start))
					})
				}
			}()

			go func() {
				for {
					hosts, err := getHosts()
					if err != nil {
						log.Fatalln(err)
					}

					if len(hosts) < workers {
						log.Printf("%v hosts -- too few for %v workers", len(hosts), workers)
					}

					for _, host := range hosts {
						select {
						case <-ctx.Done():
							return
						default:
						}
						workCh <- host
					}
				}
			}()

			wg.Wait()
			if err := syncStats(); err != nil {
				log.Fatalln(err)
			}
		},
	}

	contractsCmd = &cobra.Command{
		Use:   "contracts",
		Short: "list contracts",
		Run: func(cmd *cobra.Command, args []string) {
			renter, err := renter.New(dataDir)
			if err != nil {
				log.Fatalln(err)
			}

			contracts := renter.Contracts()
			log.Println("Contracts:", len(contracts))
			tbl := table.New("ID", "Host Key", "Expiration Height")
			for _, contract := range contracts {
				tbl.AddRow(contract.ID, contract.HostKey, contract.ExpirationHeight)
			}
			tbl.Print()
		},
	}

	hostsCmd = &cobra.Command{
		Use:   "hosts",
		Short: "list hosts",
		Run: func(cmd *cobra.Command, args []string) {
			hosts, err := getHosts()
			if err != nil {
				log.Fatalln(err)
			}
			tbl := table.New("Host Key", "Address")
			for _, host := range hosts {
				tbl.AddRow(host.PublicKey, host.Address)
			}
			tbl.Print()
		},
	}
)

func formatByteString(b uint64) string {
	const units = "KMGTPE"
	const factor = 1024

	// short-circuit for < 1024 bytes
	if b < factor {
		return fmt.Sprintf("%v bytes", b)
	}

	var i = -1
	rem := float64(b)
	for ; rem >= factor; i++ {
		rem /= factor
	}
	return fmt.Sprintf("%.2f %ciB", rem, units[i])
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

func syncStats() error {
	mu.Lock()
	defer mu.Unlock()

	tmpFile := filepath.Join(dataDir, "stats.json.tmp")
	statsFile := filepath.Join(dataDir, "stats.json")
	f, err := os.Create(tmpFile)
	if err != nil {
		return fmt.Errorf("failed to create stats tmp file: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	stats := uploadStats{
		Bytes: totalUploaded,
		Cost:  cost,
		Hosts: hostUploaded,
	}
	if err := enc.Encode(stats); err != nil {
		return fmt.Errorf("failed to encode stats: %v", err)
	} else if err := f.Sync(); err != nil {
		return fmt.Errorf("failed to sync stats: %v", err)
	} else if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close stats: %v", err)
	}
	return os.Rename(tmpFile, statsFile)
}

func mustLoadStats() {
	statsFile := filepath.Join(dataDir, "stats.json")
	f, err := os.Open(statsFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		panic(err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var stats uploadStats
	if err := dec.Decode(&stats); err != nil {
		panic(err)
	}
	totalUploaded = stats.Bytes
	cost = stats.Cost
}

func printProgress(w *wallet.SingleAddressLiteWallet, totalBytes, currentBytes uint64, cost types.Currency, duration time.Duration) {
	balance, _ := w.Balance()

	log.Printf("Uploaded: %v in %v (%v) Cost: %v Total Upload: %v Wallet Balance: %v Wallet Address %v",
		formatByteString(currentBytes),
		duration,
		formatBpsString(currentBytes, duration),
		cost.HumanString(),
		formatByteString(totalBytes),
		balance.HumanString(),
		w.Address())
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&dataDir, "data-dir", "d", ".", "data directory")
	rootCmd.Flags().IntVarP(&workers, "workers", "w", 25, "number of workers to use for uploading")
	rootCmd.AddCommand(contractsCmd, redistributeCmd, seedCmd, balanceCmd, hostsCmd)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	if err := rootCmd.Execute(); err != nil {
		log.Fatalln(err)
	}
}
