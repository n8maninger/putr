# Putr

Push data to the Sia network as fast as possible.

## Building

```
go build -o bin/ ./cmd/putr
```

## Usage
```
Available Commands:
  contracts    list contracts
  help         Help about any command
  redistribute redistribute funds
  seed         generate a new bip39 seed

Flags:
  -d, --data-dir string   data directory (default ".")
  -h, --help              help for putr
  -w, --workers int       number of workers to use for uploading (default 25)
```

## Setup

### Generate a seed 
1. Run `putr seed` to generate a bip39 seed. This seed will be used to generate
   a wallet address and sign transactions
2. Set the `PUTR_RECOVERY_PHRASE` environment variable to the generated phrase. All other
   commands will use it to load the wallet

### Fund the wallet
1. Fund the wallet address with at least 15,000 SC. the more utxos the better.
2. Wait a block for confirmation
3. Redistribute the funds with `putr redistribute` to create a large number of
   outputs. Aim to have at least as many UTXOs as workers. Each UTXO value should
   be at least 200 SC, so fund the wallet accordingly. Subtract 5 SC for miner
   fees. 100 workers should have a starting balance a little over 20,000 SC
   `100 UTXOs * 200 SC each + 5 SC miner fee = 20,005 SC`. The initial funding
   balance should last a while.
4. Wait another block for the outputs to be confirmed

### Start the upload
1. Create a data directory to store contracts `mkdir ~/putr-1`
2. Start the uploader with the desired number of workers `putr -w 100 -d ~/putr-1`

The status of the uploads will be printed to `stdout` once every minute. Raise
or lower the number of workers to saturate the connection. 100 workers should be
able to fully saturate a gigabit connection. 