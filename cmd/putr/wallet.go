package main

import (
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/n8maninger/putr/wallet"
	"github.com/spf13/cobra"
	"go.sia.tech/siad/types"
)

var (
	redistributeCmd = &cobra.Command{
		Use:   "redistribute <number of outputs> <output amount>",
		Short: "redistribute funds",
		Run: func(cmd *cobra.Command, args []string) {
			if len(args) != 2 {
				cmd.Usage()
				os.Exit(1)
			}

			count, err := strconv.ParseUint(args[0], 10, 64)
			if err != nil {
				log.Fatalln("failed to parse output count:", err)
			}
			hastings, err := types.ParseCurrency(args[1])
			if err != nil {
				log.Fatalln("failed to parse output hastings:", err)
			}
			var outputAmount types.Currency
			if _, err := fmt.Sscan(hastings, &outputAmount); err != nil {
				log.Fatalln("failed to parse output amount:", err)
			}

			w := mustLoadWallet()
			log.Println(w.Address())
			balance, err := w.Balance()
			if err != nil {
				log.Fatalln("failed to get wallet balance:", err)
			}
			log.Println(balance.HumanString())

			n, err := balance.Div(outputAmount).Uint64()
			if err != nil {
				log.Fatalln("failed to get number of outputs:", err)
			} else if n < count {
				log.Println("not enough funds to redistribute")
			}

			txn, release, err := w.Redistribute(count, outputAmount)
			if err != nil {
				log.Fatalln("failed to redistribute funds:", err)
			}
			defer release()

			log.Printf("Creating %v outputs of %v each", count, outputAmount.HumanString())
			siaCentralClient := apiClient()
			if err := siaCentralClient.BroadcastTransactionSet([]types.Transaction{txn}); err != nil {
				log.Fatalln("failed to broadcast transaction:", err)
			}
			log.Printf("Transaction %v broadcast", txn.ID())
		},
	}

	seedCmd = &cobra.Command{
		Use:   "seed",
		Short: "generate a new bip39 seed",
		Run: func(cmd *cobra.Command, args []string) {
			phrase := wallet.NewSeedPhrase()
			w, err := wallet.New(phrase)
			if err != nil {
				log.Fatalln("failed to initialize wallet:", err)
			}
			log.Println("Phrase:", phrase)
			log.Println("Address:", w.Address())
		},
	}

	balanceCmd = &cobra.Command{
		Use:   "balance",
		Short: "get wallet balance",
		Run: func(cmd *cobra.Command, args []string) {
			w := mustLoadWallet()
			balance, err := w.Balance()
			if err != nil {
				log.Fatalln("failed to get wallet balance:", err)
			}
			log.Println("Address:", w.Address())
			log.Println("Balance:", balance.HumanString())
		},
	}
)

func mustLoadWallet() *wallet.SingleAddressLiteWallet {
	recoveryPhrase := os.Getenv("PUTR_RECOVERY_PHRASE")
	if recoveryPhrase == "" {
		log.Fatalln("PUTR_RECOVERY_PHRASE environment variable not set")
	}
	wallet, err := wallet.New(recoveryPhrase)
	if err != nil {
		log.Fatalln("failed to initialize wallet:", err)
	}
	return wallet
}
