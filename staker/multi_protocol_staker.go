package staker

import (
	"context"
	"time"

	"github.com/OffchainLabs/bold/solgen/go/bridgegen"
	boldrollup "github.com/OffchainLabs/bold/solgen/go/rollupgen"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"
	"github.com/offchainlabs/nitro/staker/txbuilder"
	"github.com/offchainlabs/nitro/util/headerreader"
	"github.com/offchainlabs/nitro/util/stopwaiter"
)

var assertionCreatedId common.Hash

func init() {
	rollupAbi, err := boldrollup.RollupCoreMetaData.GetAbi()
	if err != nil {
		panic(err)
	}
	assertionCreatedEvent, ok := rollupAbi.Events["AssertionCreated"]
	if !ok {
		panic("RollupCore ABI missing AssertionCreated event")
	}
	assertionCreatedId = assertionCreatedEvent.ID
}

type MultiProtocolStaker struct {
	stopwaiter.StopWaiter
	bridge     *bridgegen.IBridge
	oldStaker  *Staker
	boldStaker *BOLDStaker
}

func NewMultiProtocolStaker(
	l1Reader *headerreader.HeaderReader,
	wallet ValidatorWalletInterface,
	callOpts bind.CallOpts,
	config L1ValidatorConfigFetcher,
	blockValidator *BlockValidator,
	statelessBlockValidator *StatelessBlockValidator,
	stakedNotifiers []LatestStakedNotifier,
	confirmedNotifiers []LatestConfirmedNotifier,
	validatorUtilsAddress common.Address,
	bridgeAddress common.Address,
	fatalErr chan<- error,
) (*MultiProtocolStaker, error) {
	oldStaker, err := NewStaker(
		l1Reader,
		wallet,
		callOpts,
		config,
		blockValidator,
		statelessBlockValidator,
		stakedNotifiers,
		confirmedNotifiers,
		validatorUtilsAddress,
		fatalErr,
	)
	if err != nil {
		return nil, err
	}
	bridge, err := bridgegen.NewIBridge(bridgeAddress, oldStaker.client)
	if err != nil {
		return nil, err
	}
	return &MultiProtocolStaker{
		oldStaker:  oldStaker,
		boldStaker: nil,
		bridge:     bridge,
	}, nil
}

func (m *MultiProtocolStaker) IsWhitelisted(ctx context.Context) (bool, error) {
	return m.oldStaker.IsWhitelisted(ctx)
}

func (m *MultiProtocolStaker) Initialize(ctx context.Context) error {
	boldActive, rollupAddress, err := m.isBoldActive(ctx)
	if err != nil {
		return err
	}
	if boldActive {
		log.Info("BOLD protocol is active, initializing BOLD staker")
		boldStaker, err := m.setupBoldStaker(ctx, rollupAddress)
		if err != nil {
			return err
		}
		m.boldStaker = boldStaker
		return m.boldStaker.Initialize(ctx)
	}
	log.Info("BOLD protocol not detected on startup, using old staker until upgrade")
	return m.oldStaker.Initialize(ctx)
}

func (m *MultiProtocolStaker) Start(ctxIn context.Context) {
	m.StopWaiter.Start(ctxIn, m)
	if m.oldStaker.Strategy() != WatchtowerStrategy {
		m.oldStaker.wallet.Start(ctxIn)
	}
	if m.boldStaker != nil {
		log.Info("Starting BOLD staker")
		m.boldStaker.Start(ctxIn)
		m.StopOnly()
	} else {
		log.Info("Starting pre-BOLD staker")
		m.oldStaker.Start(ctxIn)
		stakerSwitchInterval := m.oldStaker.config().BOLD.CheckStakerSwitchInterval
		m.CallIteratively(func(ctx context.Context) time.Duration {
			switchedToBoldProtocol, err := m.checkAndSwitchToBoldStaker(ctxIn)
			if err != nil {
				log.Error("staker: error in checking switch to bold staker", "err", err)
				return stakerSwitchInterval
			}
			if switchedToBoldProtocol {
				log.Info("Detected BOLD protocol upgrade, stopping old staker and starting BOLD staker")
				// Ready to stop the old staker.
				m.oldStaker.StopOnly()
				m.StopOnly()
			}
			return stakerSwitchInterval
		})
	}
}

func (m *MultiProtocolStaker) StopAndWait() {
	if m.boldStaker != nil {
		m.boldStaker.StopAndWait()
	}
	m.oldStaker.StopAndWait()
	m.StopWaiter.StopAndWait()
}

func (m *MultiProtocolStaker) isBoldActive(ctx context.Context) (bool, common.Address, error) {
	var addr common.Address
	if !m.oldStaker.config().BOLD.Enable {
		return false, addr, nil
	}
	callOpts := m.oldStaker.getCallOpts(ctx)
	rollupAddress, err := m.bridge.Rollup(callOpts)
	if err != nil {
		return false, addr, err
	}
	userLogic, err := boldrollup.NewRollupUserLogic(rollupAddress, m.oldStaker.client)
	if err != nil {
		return false, addr, err
	}
	_, err = userLogic.ChallengeGracePeriodBlocks(callOpts)
	// ChallengeGracePeriodBlocks only exists in the BOLD rollup contracts.
	return err == nil, rollupAddress, nil
}

func (m *MultiProtocolStaker) checkAndSwitchToBoldStaker(ctx context.Context) (bool, error) {
	shouldSwitch, rollupAddress, err := m.isBoldActive(ctx)
	if err != nil {
		return false, err
	}
	if !shouldSwitch {
		return false, nil
	}
	boldStaker, err := m.setupBoldStaker(ctx, rollupAddress)
	if err != nil {
		return false, err
	}
	m.boldStaker = boldStaker
	if err = m.boldStaker.Initialize(ctx); err != nil {
		return false, err
	}
	m.boldStaker.Start(ctx)
	return true, nil
}

func (m *MultiProtocolStaker) setupBoldStaker(
	ctx context.Context,
	rollupAddress common.Address,
) (*BOLDStaker, error) {
	txBuilder, err := txbuilder.NewBuilder(m.oldStaker.wallet)
	if err != nil {
		return nil, err
	}
	auth, err := txBuilder.Auth(ctx)
	if err != nil {
		return nil, err
	}
	boldStaker, err := newBOLDStaker(
		ctx,
		*m.oldStaker.config(),
		rollupAddress,
		*m.oldStaker.getCallOpts(ctx),
		auth,
		m.oldStaker.client,
		m.oldStaker.blockValidator,
		m.oldStaker.statelessBlockValidator,
		&m.oldStaker.config().BOLD,
		m.oldStaker.wallet.DataPoster(),
		m.oldStaker.wallet,
		m.oldStaker.stakedNotifiers,
		m.oldStaker.confirmedNotifiers,
	)
	if err != nil {
		return nil, err
	}
	return boldStaker, nil
}
