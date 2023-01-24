package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
	}
)

var (
	dataDir string
	workers int

	totalUploaded uint64
	cost          types.Currency
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

			for i := 0; i < workers; i++ {
				go uploadWorker(i, renter, wallet, workCh, resultsCh)
			}

			go func() {
				var currentUploaded uint64
				s := rate.Sometimes{First: 3, Interval: time.Minute}
				// print progress
				start := time.Now()
				for result := range resultsCh {
					totalUploaded += result.Bytes
					currentUploaded += result.Bytes
					cost = cost.Add(result.Cost)
					_ = syncStats(totalUploaded, cost)
					if result.Err != nil {
						log.Printf("worker %v error: %v", result.Worker, result.Err)
					}
					s.Do(func() { printProgress(wallet, totalUploaded, currentUploaded, cost, time.Since(start)) })
				}
			}()

			for {
				hosts, err := getHosts()
				if err != nil {
					log.Fatalln(err)
				}

				if len(hosts) < workers {
					log.Printf("%v hosts -- too few for %v workers", len(hosts), workers)
				}

				for _, host := range hosts {
					workCh <- host
				}
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

func syncStats(bytes uint64, cost types.Currency) error {
	tmpFile := filepath.Join(dataDir, "stats.json.tmp")
	statsFile := filepath.Join(dataDir, "stats.json")
	f, err := os.Create(tmpFile)
	if err != nil {
		return fmt.Errorf("failed to create stats tmp file: %v", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	stats := uploadStats{Bytes: bytes, Cost: cost}
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

	log.Printf("Uploaded: %v in %v (%v) Total Upload: %v Cost: %v Wallet Balance: %v Wallet Address %v",
		formatByteString(currentBytes),
		duration,
		formatBpsString(currentBytes, duration),
		totalBytes,
		cost.HumanString(),
		balance.HumanString(),
		w.Address())
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&dataDir, "data-dir", "d", ".", "data directory")
	rootCmd.Flags().IntVarP(&workers, "workers", "w", 25, "number of workers to use for uploading")
	rootCmd.AddCommand(contractsCmd, redistributeCmd, seedCmd, balanceCmd)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	if err := rootCmd.Execute(); err != nil {
		log.Fatalln(err)
	}
}
