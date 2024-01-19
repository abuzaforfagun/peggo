package orchestrator

import (
	"context"
	"github.com/InjectiveLabs/sdk-go/chain/peggy/types"
	"time"

	"github.com/avast/retry-go"
	"github.com/pkg/errors"
	log "github.com/xlab/suplog"

	"github.com/InjectiveLabs/metrics"

	"github.com/InjectiveLabs/peggo/orchestrator/loops"
	peggyevents "github.com/InjectiveLabs/peggo/solidity/wrappers/Peggy.sol"
)

// todo: this is outdated, need to update
// Considering blocktime of up to 3 seconds approx on the Injective Chain and an oracle loop duration = 1 minute,
// we broadcast only 20 events in each iteration.
// So better to search only 20 blocks to ensure all the events are broadcast to Injective Chain without misses.
const (
	ethBlockConfirmationDelay uint64 = 12
	defaultBlocksToSearch     uint64 = 2000
)

// EthOracleMainLoop is responsible for making sure that Ethereum events are retrieved from the Ethereum blockchain
// and ferried over to Cosmos where they will be used to issue tokens or process batches.
func (s *PeggyOrchestrator) EthOracleMainLoop(ctx context.Context) error {

	lastConfirmedEthHeight, err := s.getLastConfirmedEthHeightOnInjective(ctx)
	if err != nil {
		return err
	}

	s.logger.Debugln("last observed Ethereum block", lastConfirmedEthHeight)

	loop := ethOracleLoop{
		PeggyOrchestrator:       s,
		loopDuration:            defaultLoopDur,
		lastCheckedEthHeight:    lastConfirmedEthHeight,
		lastResyncWithInjective: time.Now(),
	}

	s.logger.WithField("loop_duration", loop.loopDuration.String()).Debugln("starting EthOracle loop...")

	return loop.Run(ctx)
}

func (s *PeggyOrchestrator) getLastConfirmedEthHeightOnInjective(ctx context.Context) (uint64, error) {
	var lastConfirmedEthHeight uint64
	getLastConfirmedEthHeightFn := func() (err error) {
		lastConfirmedEthHeight, err = s.getLastClaimBlockHeight(ctx)
		if lastConfirmedEthHeight == 0 {
			peggyParams, err := s.inj.PeggyParams(ctx)
			if err != nil {
				s.logger.WithError(err).Fatalln("unable to query peggy module params, is injectived running?")
				return err
			}

			lastConfirmedEthHeight = peggyParams.BridgeContractStartHeight
		}
		return
	}

	if err := retry.Do(getLastConfirmedEthHeightFn,
		retry.Context(ctx),
		retry.Attempts(s.maxAttempts),
		retry.OnRetry(func(n uint, err error) {
			s.logger.WithError(err).Warningf("failed to get last confirmed Ethereum height on Injective, will retry (%d)", n)
		}),
	); err != nil {
		s.logger.WithError(err).Errorln("got error, loop exits")
		return 0, err
	}

	return lastConfirmedEthHeight, nil
}

func (s *PeggyOrchestrator) getLastClaimBlockHeight(ctx context.Context) (uint64, error) {
	metrics.ReportFuncCall(s.svcTags)
	doneFn := metrics.ReportFuncTiming(s.svcTags)
	defer doneFn()

	claim, err := s.inj.LastClaimEvent(ctx)
	if err != nil {
		return 0, err
	}

	return claim.EthereumEventHeight, nil
}

type ethOracleLoop struct {
	*PeggyOrchestrator
	loopDuration            time.Duration
	lastResyncWithInjective time.Time
	lastCheckedEthHeight    uint64
}

func (l *ethOracleLoop) Logger() log.Logger {
	return l.logger.WithField("loop", "EthOracle")
}

func (l *ethOracleLoop) Run(ctx context.Context) error {
	return loops.RunLoop(ctx, l.loopDuration, func() error {
		return l.observeEthEvents(ctx)
	})
}

func (l *ethOracleLoop) observeEthEvents(ctx context.Context) error {
	newHeight, err := l.relayEvents(ctx)
	if err != nil {
		return err
	}

	l.Logger().WithFields(log.Fields{"block_start": l.lastCheckedEthHeight, "block_end": newHeight}).Debugln("scanned Ethereum blocks")
	l.lastCheckedEthHeight = newHeight

	if time.Since(l.lastResyncWithInjective) >= 48*time.Hour {
		/**
			Auto re-sync to catch up the nonce. Reasons why event nonce fall behind.
				1. It takes some time for events to be indexed on Ethereum. So if peggo queried events immediately as block produced, there is a chance the event is missed.
				   we need to re-scan this block to ensure events are not missed due to indexing delay.
				2. if validator was in UnBonding state, the claims broadcasted in last iteration are failed.
				3. if infura call failed while filtering events, the peggo missed to broadcast claim events occured in last iteration.
		**/
		if err := l.autoResync(ctx); err != nil {
			return err
		}
	}

	return nil
}

func (l *ethOracleLoop) relayEvents(ctx context.Context) (uint64, error) {
	latestHeight, err := l.getLatestEthHeight(ctx)
	if err != nil {
		return 0, err
	}

	// not enough blocks on ethereum yet
	if latestHeight <= ethBlockConfirmationDelay {
		return l.lastCheckedEthHeight, nil
	}

	// ensure that latest block has minimum confirmations
	latestHeight = latestHeight - ethBlockConfirmationDelay
	if latestHeight <= l.lastCheckedEthHeight {
		return l.lastCheckedEthHeight, nil
	}

	if latestHeight > l.lastCheckedEthHeight+defaultBlocksToSearch {
		latestHeight = l.lastCheckedEthHeight + defaultBlocksToSearch
	}

	events, err := l.getEthEvents(ctx, l.lastCheckedEthHeight, latestHeight)
	if err != nil {
		return 0, err
	}

	lastClaim, err := l.getLastEventClaim(ctx)
	if err != nil {
		return 0, err
	}

	// filter processed events
	events = events.Filter(lastClaim.EthereumEventNonce)
	if events.Num() == 0 {
		l.Logger().Infoln("no new events on Ethereum")
		return latestHeight, nil
	}

	if err := l.sendNewEventClaims(ctx, lastClaim, events); err != nil {
		return 0, err
	}

	return latestHeight, nil
}

func (l *ethOracleLoop) autoResync(ctx context.Context) error {
	var latestHeight uint64
	getLastClaimEventFn := func() (err error) {
		latestHeight, err = l.getLastClaimBlockHeight(ctx)
		return
	}

	if err := retry.Do(getLastClaimEventFn,
		retry.Context(ctx),
		retry.Attempts(l.maxAttempts),
		retry.OnRetry(func(n uint, err error) {
			l.Logger().WithError(err).Warningf("failed to get last confirmed eth height, will retry (%d)", n)
		}),
	); err != nil {
		l.Logger().WithError(err).Errorln("got error, loop exits")
		return err
	}

	l.lastCheckedEthHeight = latestHeight
	l.lastResyncWithInjective = time.Now()

	l.Logger().WithFields(log.Fields{"last_resync_time": l.lastResyncWithInjective.String(), "last_confirmed_eth_height": l.lastCheckedEthHeight}).Infoln("auto resync event nonce with Injective")

	return nil
}

func (l *ethOracleLoop) getLatestEthHeight(ctx context.Context) (uint64, error) {
	var latestHeight uint64
	getLatestEthHeightFn := func() error {
		latestHeader, err := l.eth.HeaderByNumber(ctx, nil)
		if err != nil {
			return errors.Wrap(err, "failed to get latest ethereum header")
		}

		latestHeight = latestHeader.Number.Uint64()
		return nil
	}

	if err := retry.Do(getLatestEthHeightFn,
		retry.Context(ctx),
		retry.Attempts(l.maxAttempts),
		retry.OnRetry(func(n uint, err error) {
			l.Logger().WithError(err).Warningf("failed to get latest eth header, will retry (%d)", n)
		}),
	); err != nil {
		l.Logger().WithError(err).Errorln("got error, loop exits")
		return 0, err
	}

	return latestHeight, nil
}

func (l *ethOracleLoop) getLastEventClaim(ctx context.Context) (*types.LastClaimEvent, error) {
	var claim *types.LastClaimEvent
	getLastClaimEventFn := func() error {
		c, err := l.inj.LastClaimEvent(ctx)
		if err != nil {
			return err
		}

		claim = c
		return nil
	}

	if err := retry.Do(getLastClaimEventFn,
		retry.Context(ctx),
		retry.Attempts(l.maxAttempts),
		retry.OnRetry(func(n uint, err error) {
			l.Logger().WithError(err).Warningf("failed to get last event claim, will retry (%d)", n)
		}),
	); err != nil {
		l.Logger().WithError(err).Errorln("got error, loop exits")
		return nil, err
	}

	return claim, nil
}

func (l *ethOracleLoop) sendNewEventClaims(ctx context.Context, lastClaim *types.LastClaimEvent, events ethEvents) error {
	sendClaimsFn := func() error {
		newEventNonce, err := l.inj.SendEthereumClaims(ctx,
			lastClaim.EthereumEventNonce,
			events.OldDeposits,
			events.Deposits,
			events.Withdrawals,
			events.ERC20Deployments,
			events.ValsetUpdates,
		)

		if err != nil {
			return errors.Wrap(err, "failed to send event claims to Injective")
		}

		l.Logger().WithFields(log.Fields{"claimed_events": events.Num(), "latest_event_nonce": newEventNonce}).Infoln("sent new event claims to Injective")

		return nil
	}

	if err := retry.Do(sendClaimsFn,
		retry.Context(ctx),
		retry.Attempts(l.maxAttempts),
		retry.OnRetry(func(n uint, err error) {
			l.Logger().WithError(err).Warningf("error during Ethereum event checking, will retry (%d)", n)
		}),
	); err != nil {
		l.Logger().WithError(err).Errorln("got error, loop exits")
		return err
	}

	return nil
}

type ethEvents struct {
	OldDeposits      []*peggyevents.PeggySendToCosmosEvent
	Deposits         []*peggyevents.PeggySendToInjectiveEvent
	Withdrawals      []*peggyevents.PeggyTransactionBatchExecutedEvent
	ValsetUpdates    []*peggyevents.PeggyValsetUpdatedEvent
	ERC20Deployments []*peggyevents.PeggyERC20DeployedEvent
}

func (e ethEvents) Num() int {
	return len(e.OldDeposits) + len(e.Deposits) + len(e.Withdrawals) + len(e.ValsetUpdates) + len(e.ERC20Deployments)
}

func (e ethEvents) Filter(nonce uint64) ethEvents {
	var oldDeposits []*peggyevents.PeggySendToCosmosEvent
	for _, d := range e.OldDeposits {
		if d.EventNonce.Uint64() > nonce {
			oldDeposits = append(oldDeposits, d)
		}
	}

	var deposits []*peggyevents.PeggySendToInjectiveEvent
	for _, d := range e.Deposits {
		if d.EventNonce.Uint64() > nonce {
			deposits = append(deposits, d)
		}
	}

	var withdrawals []*peggyevents.PeggyTransactionBatchExecutedEvent
	for _, w := range e.Withdrawals {
		if w.EventNonce.Uint64() > nonce {
			withdrawals = append(withdrawals, w)
		}
	}

	var valsetUpdates []*peggyevents.PeggyValsetUpdatedEvent
	for _, vs := range e.ValsetUpdates {
		if vs.EventNonce.Uint64() > nonce {
			valsetUpdates = append(valsetUpdates, vs)
		}
	}

	var erc20Deployments []*peggyevents.PeggyERC20DeployedEvent
	for _, d := range e.ERC20Deployments {
		if d.EventNonce.Uint64() > nonce {
			erc20Deployments = append(erc20Deployments, d)
		}
	}

	return ethEvents{
		OldDeposits:      oldDeposits,
		Deposits:         deposits,
		Withdrawals:      withdrawals,
		ValsetUpdates:    valsetUpdates,
		ERC20Deployments: erc20Deployments,
	}
}

func (l *ethOracleLoop) getEthEvents(ctx context.Context, startBlock, endBlock uint64) (ethEvents, error) {
	events := ethEvents{}

	scanEthEventsFn := func() error {
		legacyDeposits, err := l.eth.GetSendToCosmosEvents(startBlock, endBlock)
		if err != nil {
			return errors.Wrap(err, "failed to get SendToCosmos events")
		}

		deposits, err := l.eth.GetSendToInjectiveEvents(startBlock, endBlock)
		if err != nil {
			return errors.Wrap(err, "failed to get SendToInjective events")
		}

		withdrawals, err := l.eth.GetTransactionBatchExecutedEvents(startBlock, endBlock)
		if err != nil {
			return errors.Wrap(err, "failed to get TransactionBatchExecuted events")
		}

		erc20Deployments, err := l.eth.GetPeggyERC20DeployedEvents(startBlock, endBlock)
		if err != nil {
			return errors.Wrap(err, "failed to get ERC20Deployed events")
		}

		valsetUpdates, err := l.eth.GetValsetUpdatedEvents(startBlock, endBlock)
		if err != nil {
			return errors.Wrap(err, "failed to get ValsetUpdated events")
		}

		events.OldDeposits = legacyDeposits
		events.Deposits = deposits
		events.Withdrawals = withdrawals
		events.ValsetUpdates = valsetUpdates
		events.ERC20Deployments = erc20Deployments

		return nil
	}

	if err := retry.Do(scanEthEventsFn,
		retry.Context(ctx),
		retry.Attempts(l.maxAttempts),
		retry.OnRetry(func(n uint, err error) {
			l.Logger().WithError(err).Warningf("error during Ethereum event checking, will retry (%d)", n)
		}),
	); err != nil {
		l.Logger().WithError(err).Errorln("got error, loop exits")
		return ethEvents{}, err
	}

	return events, nil
}
