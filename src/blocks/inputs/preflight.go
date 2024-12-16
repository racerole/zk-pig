package blockinputs

import (
	"context"
	"fmt"
	"math/big"

	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	gethstate "github.com/ethereum/go-ethereum/core/state"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/hashdb"
	"github.com/kkrt-labs/kakarot-controller/pkg/ethereum"
	"github.com/kkrt-labs/kakarot-controller/pkg/ethereum/evm"
	ethrpc "github.com/kkrt-labs/kakarot-controller/pkg/ethereum/rpc"
	"github.com/kkrt-labs/kakarot-controller/pkg/ethereum/state"
	"github.com/kkrt-labs/kakarot-controller/pkg/ethereum/trie"
	"github.com/kkrt-labs/kakarot-controller/pkg/log"
	"github.com/kkrt-labs/kakarot-controller/pkg/tag"
)

// PreflightData is the data generated after performing a preflight block EVM execution without final state validation.
// This contains minimally the necessary partial state & chain data to run the full block execution + state validation
// This is an intermediate steps before generating the final ProverInputs.
type PreflightData struct {
	Block           *ethrpc.Block        `json:"block"`
	Ancestors       []*gethtypes.Header  `json:"ancestors"`
	ChainConfig     *params.ChainConfig  `json:"chainConfig"`
	Codes           []hexutil.Bytes      `json:"codes"`
	PreStateProofs  []*trie.AccountProof `json:"preStateProofs"`
	PostStateProofs []*trie.AccountProof `json:"postStateProofs"`
}

// Preflight is the interface for the preflight block execution which consists of processing an EVM block without final state validation.
// It enables to collect necessary data for necessary for later full "block processing + final state validation".
// It outputs intermediary data that will be later used to prepare necessary pre-state data for the full block execution.
type Preflight interface {
	// Preflight executes a preflight block execution and returns the intermediate PreflightExecInputs data.
	Preflight(ctx context.Context, blockNumber *big.Int) (*PreflightData, error)
}

// preflight is the implementation of the Preflight interface using an RPC remote to fetch the state datas.
type preflight struct {
	remote ethrpc.Client
}

// NewPreflight creates a new RPC Preflight instance using the provided RPC client.
func NewPreflight(remote ethrpc.Client) Preflight {
	return &preflight{
		remote: remote,
	}
}

// Preflight executes a preflight block execution and returns the intermediate PreflightExecInputs data.
func (pf *preflight) Preflight(ctx context.Context, blockNumber *big.Int) (*PreflightData, error) {
	ctx = tag.WithComponent(ctx, "preflight")
	chainCfg, block, err := pf.init(ctx, blockNumber)
	if err != nil {
		log.LoggerFromContext(ctx).WithError(err).Errorf("Failed to initialize preflight")
		return nil, fmt.Errorf("failed to initialize preflight: %v", err)
	}

	// Addd preflight tags
	ctx = tag.WithTags(
		ctx,
		tag.Key("chain.id").String(chainCfg.ChainID.String()),
		tag.Key("block.number").Int64(block.Number().Int64()),
		tag.Key("block.hash").String(block.Hash().Hex()),
	)

	// Execute preflight
	data, err := pf.preflight(ctx, chainCfg, block)
	if err != nil {
		log.LoggerFromContext(ctx).WithError(err).Errorf("Preflight failed")
		return nil, fmt.Errorf("preflight failed: %v", err)
	}
	log.LoggerFromContext(ctx).Infof("Preflight successful")
	return data, nil
}

func (pf *preflight) init(ctx context.Context, blockNumber *big.Int) (*params.ChainConfig, *gethtypes.Block, error) {
	log.LoggerFromContext(ctx).Infof("Initialize preflight...")
	chainID, err := pf.remote.ChainID(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch chain ID: %v", err)
	}

	chainCfg, err := getChainConfig(chainID)
	if err != nil {
		return nil, nil, err
	}

	block, err := pf.remote.BlockByNumber(ctx, blockNumber)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch block: %v", err)
	}

	return chainCfg, block, nil
}

type preflightContext struct {
	ctx          context.Context
	trackers     *state.AccessTrackerManager
	rpcdb        *state.RPCDatabase
	stateDB      gethstate.Database
	hc           *core.HeaderChain
	parentHeader *gethtypes.Header
}

func (pf *preflight) preflight(ctx context.Context, chainCfg *params.ChainConfig, block *gethtypes.Block) (*PreflightData, error) {
	log.LoggerFromContext(ctx).Infof("Process preflight...")

	genCtx, err := pf.prepareContext(ctx, chainCfg)
	if err != nil {
		return nil, err
	}

	if err := pf.preparePreState(genCtx, block); err != nil {
		return nil, err
	}

	execParams, err := pf.prepareProcessBlockExecParams(genCtx, block)
	if err != nil {
		return nil, err
	}

	if err := pf.execute(genCtx, execParams); err != nil {
		return nil, err
	}

	preStateProofs, deletionsPostStateProofs, err := pf.fetchStateProofs(genCtx, execParams)
	if err != nil {
		return nil, err
	}

	data := &PreflightData{
		ChainConfig:     chainCfg,
		Block:           new(ethrpc.Block).FromBlock(block, chainCfg),
		PreStateProofs:  preStateProofs,
		PostStateProofs: deletionsPostStateProofs,
	}

	witness := execParams.State.Witness()
	for code := range witness.Codes {
		data.Codes = append(data.Codes, []byte(code))
	}

	data.Ancestors = witness.Headers

	return data, nil
}

func (pf *preflight) prepareContext(ctx context.Context, chainCfg *params.ChainConfig) (*preflightContext, error) {
	log.LoggerFromContext(ctx).Debug("Prepare context for block execution...")

	trackers := state.NewAccessTrackerManager()
	db := rawdb.NewMemoryDatabase()
	trieDB := triedb.NewDatabase(db, &triedb.Config{HashDB: &hashdb.Config{}})
	rpcdb := state.NewRPCDatabase(gethstate.NewDatabase(trieDB, nil), pf.remote)
	stateDB := state.NewAccessTrackerDatabase(rpcdb, trackers)

	hc, err := ethereum.NewChain(chainCfg, stateDB)
	if err != nil {
		return nil, fmt.Errorf("failed to create chain: %v", err)
	}

	return &preflightContext{
		ctx:      ctx,
		trackers: trackers,
		stateDB:  stateDB,
		rpcdb:    rpcdb,
		hc:       hc,
	}, nil
}

func (pf *preflight) preparePreState(ctx *preflightContext, block *gethtypes.Block) error {
	log.LoggerFromContext(ctx.ctx).Info("Prepare pre-state... (this may take a while)")
	// --- Preload the 256 ancestors of the block necessary for BLOCKHASH opcode ---
	// TODO: we currently preload all the blocks which is overkill as the block execution will very rarely access those ancestors.
	// We should optimize this by fetching ancestors at block execution time
	// An approach might consist in implementing a rpc wrapper over ethdb.KeyValueReader that fetches the ancestors on demand
	ancestors, err := ethereum.FillDBWithAncestors(ctx.ctx, ctx.stateDB.TrieDB().Disk(), pf.remote.HeaderByHash, block, 256)
	if err != nil {
		return fmt.Errorf("failed to fill db with block ancestors: %v", err)
	}

	// Mark the parent block so the RPC database can work effectively
	ctx.parentHeader = ancestors[0]
	ctx.rpcdb.MarkBlock(ctx.parentHeader)

	return nil
}

func (pf *preflight) prepareProcessBlockExecParams(ctx *preflightContext, block *gethtypes.Block) (*evm.ExecParams, error) {
	log.LoggerFromContext(ctx.ctx).Debug("Prepare execution parameters... (this may take a while)")

	st, err := gethstate.New(ctx.parentHeader.Root, ctx.stateDB)
	if err != nil {
		return nil, fmt.Errorf("failed to create state from parent root %v: %v", ctx.parentHeader.Root, err)
	}

	return &evm.ExecParams{
		Block:    block,
		Validate: false, // We do not validate on because we don't have a proper pre-state yet
		VMConfig: &vm.Config{
			StatelessSelfValidation: true, // We enable stateless self-validation so witness data is filled
		},
		Chain: ctx.hc,
		State: st,
	}, nil
}

// execute runs the actual block EVM execution
func (pf *preflight) execute(ctx *preflightContext, execParams *evm.ExecParams) error {
	log.LoggerFromContext(ctx.ctx).Infof("Execute EVM... (this may take a while)")
	_, err := evm.ExecutorWithTags("evm")(evm.ExecutorWithLog()(evm.NewExecutor())).Execute(ctx.ctx, execParams)
	if err != nil {
		return fmt.Errorf("failed to execute block: %v", err)
	}

	return nil
}

// fetchStateProofs for all accounts and storage slots that were accessed during the block execution
// It fetches the state proofs both at the initial state (parent state) and at the final state
func (pf *preflight) fetchStateProofs(ctx *preflightContext, execParams *evm.ExecParams) (preStateProofs, postStateProofs []*trie.AccountProof, err error) {
	log.LoggerFromContext(ctx.ctx).Infof("Fetch state proofs after successful EVM execution... (this may take a while)")

	finalState := execParams.State
	tracker := ctx.trackers.GetTracker(ctx.parentHeader.Root)
	for account := range tracker.Accounts {
		var (
			slots       = []string{}
			deletedSlot = []string{}
		)
		if storage, ok := tracker.Storage[account]; ok {
			for slot, preStateValue := range storage {
				slots = append(slots, slot.Hex())
				if (preStateValue != gethcommon.Hash{}) && (finalState.GetState(account, slot) == gethcommon.Hash{}) {
					deletedSlot = append(deletedSlot, slot.Hex())
				}
			}
		}

		// Get proofs for every accounts on the initial state (parent state)
		acc, err := pf.remote.GetProof(ctx.ctx, account, slots, ctx.parentHeader.Number)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get proof for account %v: %v", account, err)
		}
		preStateProofs = append(preStateProofs, trie.AccountProofFromRPC(acc))

		// Also get necessary proofs at final state
		if len(deletedSlot) == 0 && !finalState.HasSelfDestructed(account) {
			// Account was not deleted so we don't need to fetch post-state proofs for it
			continue
		}

		// Also get proofs at final state for deleted accounts & slots
		acc, err = pf.remote.GetProof(ctx.ctx, account, deletedSlot, execParams.Block.Number())
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get proof for account %v: %v", account, err)
		}
		postStateProofs = append(postStateProofs, trie.AccountProofFromRPC(acc))
	}

	return preStateProofs, postStateProofs, nil
}
