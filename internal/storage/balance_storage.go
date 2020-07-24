package storage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/big"

	"github.com/coinbase/rosetta-cli/internal/utils"

	"github.com/coinbase/rosetta-sdk-go/fetcher"
	"github.com/coinbase/rosetta-sdk-go/parser"
	"github.com/coinbase/rosetta-sdk-go/reconciler"
	"github.com/coinbase/rosetta-sdk-go/types"
)

var _ BlockWorker = (*BalanceStorage)(nil)

const (
	// balanceNamespace is prepended to any stored balance.
	balanceNamespace = "balance"
)

var (
	// ErrAccountNotFound is returned when an account
	// is not found in BalanceStorage.
	ErrAccountNotFound = errors.New("account not found")

	// ErrNegativeBalance is returned when an account
	// balance goes negative as the result of an operation.
	ErrNegativeBalance = errors.New("negative balance")
)

/*
  Key Construction
*/

// GetBalanceKey returns a deterministic hash of an types.Account + types.Currency.
func GetBalanceKey(account *types.AccountIdentifier, currency *types.Currency) []byte {
	return []byte(
		fmt.Sprintf("%s/%s/%s", balanceNamespace, types.Hash(account), types.Hash(currency)),
	)
}

type BalanceHandler interface {
	BlockAdded(ctx context.Context, block *types.Block, changes []*parser.BalanceChange) error
	BlockRemoved(ctx context.Context, block *types.Block, changes []*parser.BalanceChange) error
}

// BalanceStorage implements block specific storage methods
// on top of a Database and DatabaseTransaction interface.
type BalanceStorage struct {
	db Database

	network *types.NetworkIdentifier
	fetcher *fetcher.Fetcher
	parser  *parser.Parser
	handler BalanceHandler

	// Configuration settings
	lookupBalanceByBlock bool
	exemptAccounts       map[string]struct{}
}

// NewBalanceStorage returns a new BalanceStorage.
func NewBalanceStorage(
	db Database,
	network *types.NetworkIdentifier,
	fetcher *fetcher.Fetcher,
	lookupBalanceByBlock bool,
	exemptAccounts []*reconciler.AccountCurrency,
	handler BalanceHandler,
) *BalanceStorage {
	exemptMap := map[string]struct{}{}

	// Pre-process exemptAccounts on initialization
	// to provide fast lookup while syncing.
	for _, account := range exemptAccounts {
		exemptMap[types.Hash(account)] = struct{}{}
	}

	b := &BalanceStorage{
		db:                   db,
		network:              network,
		fetcher:              fetcher,
		lookupBalanceByBlock: lookupBalanceByBlock,
		exemptAccounts:       exemptMap,
		handler:              handler,
	}

	b.parser = parser.New(fetcher.Asserter, b.ExemptFunc())

	return b
}

// ExemptFunc returns a parser.ExemptOperation.
func (b *BalanceStorage) ExemptFunc() parser.ExemptOperation {
	return func(op *types.Operation) bool {
		thisAcct := types.Hash(&reconciler.AccountCurrency{
			Account:  op.Account,
			Currency: op.Amount.Currency,
		})

		_, exists := b.exemptAccounts[thisAcct]
		return exists
	}
}

func (b *BalanceStorage) AddingBlock(ctx context.Context, block *types.Block, transaction DatabaseTransaction) (CommitWorker, error) {
	changes, err := b.parser.BalanceChanges(ctx, block, false)
	if err != nil {
		return nil, fmt.Errorf("%w: unable to calculate balance changes", err)
	}

	for _, change := range changes {
		if err := b.UpdateBalance(ctx, transaction, change, block.ParentBlockIdentifier); err != nil {
			return nil, err
		}
	}

	return func(ctx context.Context) error {
		return b.handler.BlockAdded(ctx, block, changes)
	}, nil
}

func (b *BalanceStorage) RemovingBlock(ctx context.Context, block *types.Block, transaction DatabaseTransaction) (CommitWorker, error) {
	changes, err := b.parser.BalanceChanges(ctx, block, true)
	if err != nil {
		return nil, fmt.Errorf("%w: unable to calculate balance changes", err)
	}

	for _, change := range changes {
		if err := b.UpdateBalance(ctx, transaction, change, block.BlockIdentifier); err != nil {
			return nil, err
		}
	}

	return func(ctx context.Context) error {
		return b.handler.BlockRemoved(ctx, block, changes)
	}, nil
}

type balanceEntry struct {
	Account *types.AccountIdentifier `json:"account"`
	Amount  *types.Amount            `json:"amount"`
	Block   *types.BlockIdentifier   `json:"block"`
}

// SetBalance allows a client to set the balance of an account in a database
// transaction. This is particularly useful for bootstrapping balances.
func (b *BalanceStorage) SetBalance(
	ctx context.Context,
	dbTransaction DatabaseTransaction,
	account *types.AccountIdentifier,
	amount *types.Amount,
	block *types.BlockIdentifier,
) error {
	key := GetBalanceKey(account, amount.Currency)

	serialBal, err := encode(&balanceEntry{
		Account: account,
		Amount:  amount,
		Block:   block,
	})
	if err != nil {
		return err
	}

	if err := dbTransaction.Set(ctx, key, serialBal); err != nil {
		return err
	}

	return nil
}

func (b *BalanceStorage) AccountBalance(
	ctx context.Context,
	account *types.AccountIdentifier,
	currency *types.Currency,
	block *types.BlockIdentifier,
) (*types.Amount, error) {
	if !b.lookupBalanceByBlock {
		return &types.Amount{
			Value:    "0",
			Currency: currency,
		}, nil
	}

	// In the case that we are syncing from arbitrary height,
	// we may need to recover the balance of an account to
	// perform validations.
	_, value, err := reconciler.GetCurrencyBalance(
		ctx,
		b.fetcher,
		b.network,
		account,
		currency,
		types.ConstructPartialBlockIdentifier(block),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: unable to get currency balance in storage helper", err)
	}

	return &types.Amount{
		Value:    value,
		Currency: currency,
	}, nil
}

// UpdateBalance updates a types.AccountIdentifer
// by a types.Amount and sets the account's most
// recent accessed block.
func (b *BalanceStorage) UpdateBalance(
	ctx context.Context,
	dbTransaction DatabaseTransaction,
	change *parser.BalanceChange,
	parentBlock *types.BlockIdentifier,
) error {
	if change.Currency == nil {
		return errors.New("invalid currency")
	}

	key := GetBalanceKey(change.Account, change.Currency)
	// Get existing balance on key
	exists, balance, err := dbTransaction.Get(ctx, key)
	if err != nil {
		return err
	}

	var existingValue string
	switch {
	case exists:
		// This could happen if balances are bootstrapped and should not be
		// overridden.
		var bal balanceEntry
		err := decode(balance, &bal)
		if err != nil {
			return err
		}

		existingValue = bal.Amount.Value
	case parentBlock != nil && change.Block.Hash == parentBlock.Hash:
		// Don't attempt to use the helper if we are going to query the same
		// block we are processing (causes the duplicate issue).
		existingValue = "0"
	default:
		// Use helper to fetch existing balance.
		amount, err := b.AccountBalance(ctx, change.Account, change.Currency, parentBlock)
		if err != nil {
			return fmt.Errorf("%w: unable to get previous account balance", err)
		}

		existingValue = amount.Value
	}

	newVal, err := types.AddValues(change.Difference, existingValue)
	if err != nil {
		return err
	}

	bigNewVal, ok := new(big.Int).SetString(newVal, 10)
	if !ok {
		return fmt.Errorf("%s is not an integer", newVal)
	}

	if bigNewVal.Sign() == -1 {
		return fmt.Errorf(
			"%w %s:%+v for %+v at %+v",
			ErrNegativeBalance,
			newVal,
			change.Currency,
			change.Account,
			change.Block,
		)
	}

	serialBal, err := encode(&balanceEntry{
		Account: change.Account,
		Amount: &types.Amount{
			Value:    newVal,
			Currency: change.Currency,
		},
		Block: change.Block,
	})
	if err != nil {
		return err
	}

	return dbTransaction.Set(ctx, key, serialBal)
}

// GetBalance returns all the balances of a types.AccountIdentifier
// and the types.BlockIdentifier it was last updated at.
func (b *BalanceStorage) GetBalance(
	ctx context.Context,
	account *types.AccountIdentifier,
	currency *types.Currency,
	headBlock *types.BlockIdentifier,
) (*types.Amount, *types.BlockIdentifier, error) {
	transaction := b.db.NewDatabaseTransaction(ctx, false)
	defer transaction.Discard(ctx)

	key := GetBalanceKey(account, currency)
	exists, bal, err := transaction.Get(ctx, key)
	if err != nil {
		return nil, nil, err
	}

	// When beginning syncing from an arbitrary height, an account may
	// not yet have a cached balance when requested. If this is the case,
	// we fetch the balance from the node for the given height and persist
	// it. This is particularly useful when monitoring interesting accounts.
	if !exists {
		amount, err := b.AccountBalance(ctx, account, currency, headBlock)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: unable to get account balance from helper", err)
		}

		writeTransaction := b.db.NewDatabaseTransaction(ctx, true)
		defer writeTransaction.Discard(ctx)
		err = b.SetBalance(
			ctx,
			writeTransaction,
			account,
			amount,
			headBlock,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: unable to set account balance", err)
		}

		err = writeTransaction.Commit(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: unable to commit account balance transaction", err)
		}

		return amount, headBlock, nil
	}

	var popBal balanceEntry
	err = decode(bal, &popBal)
	if err != nil {
		return nil, nil, err
	}

	return popBal.Amount, popBal.Block, nil
}

// BootstrapBalance represents a balance of
// a *types.AccountIdentifier and a *types.Currency in the
// genesis block.
// TODO: Must be exported for use
type BootstrapBalance struct {
	Account  *types.AccountIdentifier `json:"account_identifier,omitempty"`
	Currency *types.Currency          `json:"currency,omitempty"`
	Value    string                   `json:"value,omitempty"`
}

// BootstrapBalances is utilized to set the balance of
// any number of AccountIdentifiers at the genesis blocks.
// This is particularly useful for setting the value of
// accounts that received an allocation in the genesis block.
func (b *BalanceStorage) BootstrapBalances(
	ctx context.Context,
	bootstrapBalancesFile string,
	genesisBlockIdentifier *types.BlockIdentifier,
) error {
	// Read bootstrap file
	balances := []*BootstrapBalance{}
	if err := utils.LoadAndParse(bootstrapBalancesFile, &balances); err != nil {
		return err
	}

	// TODO: check outside of this function
	// _, err := blockStorage.GetHeadBlockIdentifier(ctx)
	// if err != ErrHeadBlockNotFound {
	// 	log.Println("Skipping balance bootstrapping because already started syncing")
	// 	return nil
	// }

	// Update balances in database
	dbTransaction := b.db.NewDatabaseTransaction(ctx, true)
	defer dbTransaction.Discard(ctx)

	for _, balance := range balances {
		// Ensure change.Difference is valid
		amountValue, ok := new(big.Int).SetString(balance.Value, 10)
		if !ok {
			return fmt.Errorf("%s is not an integer", balance.Value)
		}

		if amountValue.Sign() < 1 {
			return fmt.Errorf("cannot bootstrap zero or negative balance %s", amountValue.String())
		}

		log.Printf(
			"Setting account %s balance to %s %+v\n",
			balance.Account.Address,
			balance.Value,
			balance.Currency,
		)

		err := b.SetBalance(
			ctx,
			dbTransaction,
			balance.Account,
			&types.Amount{
				Value:    balance.Value,
				Currency: balance.Currency,
			},
			genesisBlockIdentifier,
		)
		if err != nil {
			return err
		}
	}

	err := dbTransaction.Commit(ctx)
	if err != nil {
		return err
	}

	log.Printf("%d Balances Bootstrapped\n", len(balances))
	return nil
}

// GetAllAccountCurrency scans the db for all balances and returns a slice
// of reconciler.AccountCurrency. This is useful for bootstrapping the reconciler
// after restart.
func (b *BalanceStorage) GetAllAccountCurrency(
	ctx context.Context,
) ([]*reconciler.AccountCurrency, error) {
	rawBalances, err := b.db.Scan(ctx, []byte(balanceNamespace))
	if err != nil {
		return nil, fmt.Errorf("%w database scan failed", err)
	}

	accounts := make([]*reconciler.AccountCurrency, len(rawBalances))
	for i, rawBalance := range rawBalances {
		var deserialBal balanceEntry
		err := decode(rawBalance, &deserialBal)
		if err != nil {
			return nil, fmt.Errorf(
				"%w unable to parse balance entry for %s",
				err,
				string(rawBalance),
			)
		}

		accounts[i] = &reconciler.AccountCurrency{
			Account:  deserialBal.Account,
			Currency: deserialBal.Amount.Currency,
		}
	}

	return accounts, nil
}