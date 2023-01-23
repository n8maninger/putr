package main

import (
	"fmt"
	"log"
	"time"

	"github.com/n8maninger/putr/renter"
	"github.com/n8maninger/putr/wallet"
	"github.com/rodaine/table"
	"github.com/spf13/cobra"
	"go.sia.tech/siad/types"
	"golang.org/x/time/rate"
)

var (
	dataDir string
	workers int
)

var (
	rootCmd = &cobra.Command{
		Use:   "putr",
		Short: "putr is a command line tool for uploading files to the Sia network",
		Run: func(cmd *cobra.Command, args []string) {
			workCh := make(chan host, workers)
			resultsCh := make(chan uploadResult, 1)

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
				s := rate.Sometimes{First: 3, Interval: time.Minute}
				// print progress
				var uploaded uint64
				var cost types.Currency
				start := time.Now()
				for result := range resultsCh {
					uploaded += result.Bytes
					cost = cost.Add(result.Cost)
					if result.Err != nil {
						log.Printf("worker %v error: %v", result.Worker, result.Err)
					}
					s.Do(func() { printProgress(wallet, uploaded, cost, time.Since(start)) })
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

func printProgress(w *wallet.SingleAddressLiteWallet, bytes uint64, cost types.Currency, duration time.Duration) {
	balance, _ := w.Balance()

	log.Printf("Uploaded: %v in %v (%v) Cost: %v Wallet Balance: %v Wallet Address %v",
		formatByteString(bytes),
		duration,
		formatBpsString(bytes, duration),
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
