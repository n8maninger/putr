package renter

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/n8maninger/putr/wallet"
	"go.sia.tech/renterd/rhp/v2"
	"go.sia.tech/siad/crypto"
	"go.sia.tech/siad/types"
	"lukechampine.com/frand"
)

type (
	saveMeta struct {
		RenterKey rhp.PrivateKey `json:"renterKey"`
		Contracts []ContractMeta `json:"contracts"`
	}

	// ContractMeta tracks the metadata of a storage contract.
	ContractMeta struct {
		ID               types.FileContractID `json:"id"`
		HostKey          rhp.PublicKey        `json:"hostKey"`
		ExpirationHeight uint64               `json:"expirationHeight"`
	}

	// A Wallet funds and signs transactions.
	Wallet interface {
		Address() types.UnlockHash
		FundTransaction(txn *types.Transaction, amount types.Currency) ([]crypto.Hash, func(), error)
		SignTransaction(txn *types.Transaction, toSign []crypto.Hash, cf types.CoveredFields) error
	}

	// A Renter is a helper type that manages the formation of contracts and rhp
	// sessions.
	Renter struct {
		renterKey rhp.PrivateKey
		dir       string

		close chan struct{}

		mu            sync.Mutex
		locked        map[rhp.PublicKey]bool
		currentHeight uint64
		fee           types.Currency
		contracts     map[rhp.PublicKey]ContractMeta
	}
)

// ErrNoContract is returned if no contract has been formed with a requested host
var ErrNoContract = errors.New("no contract formed")

func (r *Renter) refreshHeight() error {
	client := apiClient()
	tip, err := client.GetChainIndex()
	if err != nil {
		return fmt.Errorf("failed to get consensus state: %w", err)
	}
	// estimate miner fee
	_, max, err := client.GetTransactionFees()
	if err != nil {
		return fmt.Errorf("failed to get transaction fees: %w", err)
	}
	r.mu.Lock()
	r.currentHeight = tip.Height
	r.fee = max
	r.mu.Unlock()
	return nil
}

// FormContract attempts to form a contract with the host.
func (r *Renter) FormContract(hostKey rhp.PublicKey, address string, downloadAmount, uploadAmount, duration uint64, w Wallet) (ContractMeta, func(), error) {
	r.mu.Lock()
	currentHeight := r.currentHeight
	max := r.fee
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	t, err := dialTransport(ctx, address, hostKey)
	if err != nil {
		return ContractMeta{}, nil, fmt.Errorf("failed to dial host: %w", err)
	}
	defer t.Close()

	settings, err := rhp.RPCSettings(ctx, t)
	if err != nil {
		return ContractMeta{}, nil, fmt.Errorf("failed to get host settings: %w", err)
	}

	// estimate the funding required
	renterFunds := settings.ContractPrice
	renterFunds = renterFunds.Add(settings.UploadBandwidthPrice.Mul64(uploadAmount))
	renterFunds = renterFunds.Add(settings.DownloadBandwidthPrice.Mul64(downloadAmount))
	renterFunds = renterFunds.Add(settings.StoragePrice.Mul64(uploadAmount).Mul64(duration))
	renterFunds = renterFunds.MulFloat(1.1) // add 10% buffer

	hostCollateral := settings.Collateral.Mul64(uploadAmount).Mul64(duration)

	// create the contract
	contract := rhp.PrepareContractFormation(r.renterKey, hostKey, renterFunds, hostCollateral, currentHeight+duration, settings, w.Address())

	fee := max.Mul64(1200)
	formationCost := rhp.ContractFormationCost(contract, settings.ContractPrice)
	// fund and sign the formation transaction
	formationTxn := types.Transaction{
		MinerFees:     []types.Currency{fee},
		FileContracts: []types.FileContract{contract},
	}
	toSign, release, err := w.FundTransaction(&formationTxn, formationCost.Add(fee))
	if err != nil {
		return ContractMeta{}, nil, fmt.Errorf("failed to fund transaction: %w", err)
	}
	defer release()
	if err := w.SignTransaction(&formationTxn, toSign, wallet.ExplicitCoveredFields(formationTxn)); err != nil {
		return ContractMeta{}, nil, fmt.Errorf("failed to sign transaction: %w", err)
	}

	renterContract, _, err := rhp.RPCFormContract(t, r.renterKey, []types.Transaction{formationTxn})
	if err != nil {
		return ContractMeta{}, nil, fmt.Errorf("failed to form contract: %w", err)
	}
	meta := ContractMeta{
		ID:               renterContract.ID(),
		HostKey:          hostKey,
		ExpirationHeight: uint64(renterContract.Revision.NewWindowStart) - 5,
	}

	if err := r.save(); err != nil {
		return ContractMeta{}, nil, fmt.Errorf("failed to save renter: %w", err)
	}

	r.mu.Lock()
	r.contracts[hostKey] = meta
	r.locked[hostKey] = true
	r.mu.Unlock()
	return meta, func() {
		r.mu.Lock()
		delete(r.locked, hostKey)
		r.mu.Unlock()
	}, nil
}

// save atomically saves the renter state to disk.
func (r *Renter) save() error {
	if err := os.MkdirAll(r.dir, 0700); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}
	meta := saveMeta{
		RenterKey: r.renterKey,
		Contracts: make([]ContractMeta, 0, len(r.contracts)),
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, contract := range r.contracts {
		if contract.ExpirationHeight < r.currentHeight {
			continue
		}
		meta.Contracts = append(meta.Contracts, contract)
	}

	tmpFile := filepath.Join(r.dir, "contracts.json.tmp")
	outputFile := filepath.Join(r.dir, "contracts.json")
	f, err := os.Create(tmpFile)
	if err != nil {
		return fmt.Errorf("failed to open contracts file: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(meta); err != nil {
		return fmt.Errorf("failed to encode contracts: %w", err)
	}
	// sync and automically replace the old file
	if err := f.Sync(); err != nil {
		return fmt.Errorf("failed to sync contracts file: %w", err)
	} else if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close contracts file: %w", err)
	} else if err := os.Rename(tmpFile, outputFile); err != nil {
		return fmt.Errorf("failed to rename contracts file: %w", err)
	}
	return nil
}

// load loads the renter state from disk.
func (r *Renter) load() error {
	inputFile := filepath.Join(r.dir, "contracts.json")
	f, err := os.Open(inputFile)
	if err != nil {
		return fmt.Errorf("failed to open contracts file: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	var meta saveMeta
	if err := dec.Decode(&meta); err != nil {
		return fmt.Errorf("failed to decode contracts: %w", err)
	}
	r.renterKey = meta.RenterKey
	r.mu.Lock()
	r.contracts = make(map[rhp.PublicKey]ContractMeta)
	for _, contract := range meta.Contracts {
		if contract.ExpirationHeight <= r.currentHeight {
			continue
		}
		r.contracts[contract.HostKey] = contract
	}
	r.mu.Unlock()
	if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close contracts file: %w", err)
	} else if err := r.save(); err != nil { // prune expired contracts
		return fmt.Errorf("failed to prune contracts: %w", err)
	}
	return nil
}

// HostContract returns the contract for a host. release should be called
// when the contract is no longer needed.
func (r *Renter) HostContract(hostID rhp.PublicKey) (_ ContractMeta, release func(), _ error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	meta, ok := r.contracts[hostID]
	currentHeight := r.currentHeight

	if r.locked[hostID] {
		return ContractMeta{}, nil, errors.New("contract is locked")
	}

	// check that a contract exists and has not expired
	if !ok || meta.ExpirationHeight <= currentHeight {
		return ContractMeta{}, nil, ErrNoContract
	}
	r.locked[hostID] = true
	return meta, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.locked, hostID)
	}, nil
}

// Height returns the current height of the renter.
func (r *Renter) Height() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentHeight
}

// Hosts returns all hosts that the renter has a contract with
func (r *Renter) Hosts() []rhp.PublicKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	var hosts []rhp.PublicKey
	for _, meta := range r.contracts {
		if meta.ExpirationHeight > r.currentHeight {
			hosts = append(hosts, meta.HostKey)
		}
	}
	return hosts
}

// Contracts returns all contracts that the renter has
func (r *Renter) Contracts() (contracts []ContractMeta) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, meta := range r.contracts {
		contracts = append(contracts, meta)
	}
	return contracts
}

// RemoveHostContract deletes the contract for a host.
func (r *Renter) RemoveHostContract(hostID rhp.PublicKey) error {
	r.mu.Lock()
	delete(r.contracts, hostID)
	r.mu.Unlock()
	return r.save()
}

// NewSession initializes a new rhp session with the given host and locks the
// contract.
func (r *Renter) NewSession(ctx context.Context, hostPub rhp.PublicKey, address string, contractID types.FileContractID) (*rhp.Session, error) {
	return rhp.DialSession(ctx, address, hostPub, contractID, r.renterKey)
}

// Close closes the renter
func (r *Renter) Close() {
	select {
	case <-r.close:
		return
	default:
		close(r.close)
	}
	r.save()
}

// New initializes a new lite-renter that uses the Sia Central API for
// consensus.
func New(dir string) (*Renter, error) {
	r := &Renter{
		renterKey: rhp.PrivateKey(ed25519.NewKeyFromSeed(frand.Bytes(ed25519.SeedSize))),
		dir:       dir,

		close:     make(chan struct{}),
		locked:    make(map[rhp.PublicKey]bool),
		contracts: make(map[rhp.PublicKey]ContractMeta),
	}
	// get the current block height
	if err := r.refreshHeight(); err != nil {
		return nil, fmt.Errorf("failed to get block height: %w", err)
	}
	// batch height requests
	t := time.NewTicker(15 * time.Second)
	go func() {
		for {
			select {
			case <-r.close:
				t.Stop()
				return
			case <-t.C:
			}

			// update the renter's block height, ignore the error
			r.refreshHeight()
		}
	}()

	// renter key and contracts will be overwritten if the file exists
	if err := r.load(); !errors.Is(err, os.ErrNotExist) && err != nil {
		return nil, fmt.Errorf("failed to load contracts: %w", err)
	}
	return r, nil
}
