package kaspastratum

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/onemorebsmith/kaspastratum/src/gostratum"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

var bigJobRegex = regexp.MustCompile(".*(BzMiner|IceRiverMiner).*")

const balanceDelay = time.Minute

type clientListener struct {
	logger           *zap.SugaredLogger
	shareHandler     *shareHandler
	clientLock       sync.RWMutex
	clients          map[int32]*gostratum.StratumContext
	activeWorkers    map[string]int32 // maps "wallet.worker" to client ID
	lastBalanceCheck time.Time
	clientCounter    int32
	minShareDiff     float64
	extranonceSize   int8
	maxExtranonce    int32
	nextExtranonce   int32
	lastWorkerDiff   map[string]float64 // NEW: stores last vardiff per worker
}

func newClientListener(logger *zap.SugaredLogger, shareHandler *shareHandler, minShareDiff float64, extranonceSize int8) *clientListener {
	return &clientListener{
		logger:         logger,
		minShareDiff:   minShareDiff,
		extranonceSize: extranonceSize,
		maxExtranonce:  int32(math.Pow(2, (8*math.Min(float64(extranonceSize), 3))) - 1),
		nextExtranonce: 0,
		clientLock:     sync.RWMutex{},
		shareHandler:   shareHandler,
		clients:        make(map[int32]*gostratum.StratumContext),
		activeWorkers:  make(map[string]int32),
		lastWorkerDiff: make(map[string]float64), // NEW
	}
}

func (c *clientListener) IsWorkerActive(wallet, worker string) bool {
	c.clientLock.Lock()
	defer c.clientLock.Unlock()
	
	workerKey := fmt.Sprintf("%s.%s", wallet, worker)
	_, exists := c.activeWorkers[workerKey]
	return exists
}

func (c *clientListener) RegisterWorker(wallet, worker string, clientId int32) bool {
	c.clientLock.Lock()
	defer c.clientLock.Unlock()

	workerKey := fmt.Sprintf("%s.%s", wallet, worker)
	if oldClientId, exists := c.activeWorkers[workerKey]; exists {
		if oldClient, clientExists := c.clients[oldClientId]; clientExists {
			c.logger.Debug("disconnecting old client for worker", zap.String("worker", workerKey), zap.Int32("old_client_id", oldClientId), zap.Int32("new_client_id", clientId))
			go oldClient.Disconnect()
		}
	}
	c.activeWorkers[workerKey] = clientId
	return true
}

func (c *clientListener) OnConnect(ctx *gostratum.StratumContext) {
	var extranonce int32

	idx := atomic.AddInt32(&c.clientCounter, 1)
	ctx.Id = idx
	c.clientLock.Lock()
	if c.extranonceSize > 0 {
		extranonce = c.nextExtranonce
		if c.nextExtranonce < c.maxExtranonce {
			c.nextExtranonce++
		} else {
			c.nextExtranonce = 0
			c.logger.Warn("wrapped extranonce! new clients may be duplicating work...")
		}
	}
	c.clients[idx] = ctx
	c.clientLock.Unlock()
	ctx.Logger = ctx.Logger.With(zap.Int("client_id", int(ctx.Id)))

	if c.extranonceSize > 0 {
		ctx.Extranonce = fmt.Sprintf("%0*x", c.extranonceSize*2, extranonce)
	}

	// Create a new MiningState for this client
	ctx.State = MiningStateGenerator()

	go func() {
		// hacky, but give time for the authorize to go through so we can use the worker name
		time.Sleep(5 * time.Second)
		c.shareHandler.getCreateStats(ctx) // create the stats if they don't exist
		RecordMinerConnect(ctx)
	}()

	// Restore last vardiff if available
	if ctx.WalletAddr != "" && ctx.WorkerName != "" {
		workerKey := fmt.Sprintf("%s.%s", ctx.WalletAddr, ctx.WorkerName)
		c.clientLock.Lock()
		if lastDiff, ok := c.lastWorkerDiff[workerKey]; ok && lastDiff > 0 {
			state := GetMiningState(ctx)
			if state.stratumDiff == nil {
				state.stratumDiff = newKaspaDiff()
			}
			state.stratumDiff.setDiffValue(lastDiff)
			c.shareHandler.setClientVardiff(ctx, lastDiff)
			c.logger.Info("Restored last vardiff for worker", zap.String("worker", workerKey), zap.Float64("diff", lastDiff))
		}
		c.clientLock.Unlock()
	}
}

func (c *clientListener) OnDisconnect(ctx *gostratum.StratumContext) {
	ctx.Done()
	c.clientLock.Lock()
	delete(c.clients, ctx.Id)
	// Remove from active workers map
	if ctx.WalletAddr != "" && ctx.WorkerName != "" {
		workerKey := fmt.Sprintf("%s.%s", ctx.WalletAddr, ctx.WorkerName)
		if id, ok := c.activeWorkers[workerKey]; ok && id == ctx.Id {
			delete(c.activeWorkers, workerKey)
		}
		// Save last vardiff for this worker
		stats := c.shareHandler.getCreateStats(ctx)
		c.lastWorkerDiff[workerKey] = stats.MinDiff.Load()
	}
	c.logger.Debug("removed client", zap.Int32("client_id", ctx.Id))
	c.clientLock.Unlock()
	RecordDisconnect(ctx)
	RecordMinerDisconnect(ctx)
}

func (c *clientListener) NewBlockAvailable(kapi *KaspaApi) {
	c.clientLock.Lock()
	addresses := make([]string, 0, len(c.clients))
	for _, cl := range c.clients {
		if !cl.Connected() {
			continue
		}
		go func(client *gostratum.StratumContext) {
			// ensure the client is still active before trying to do anything
			if client.WalletAddr != "" && client.WorkerName != "" && !c.IsWorkerActive(client.WalletAddr, client.WorkerName) {
				return
			}

			state := GetMiningState(client)
			if client.WalletAddr == "" {
				if time.Since(state.connectTime) > time.Second*20 { // timeout passed
					// this happens pretty frequently in gcp/aws land since script-kiddies scrape ports
					client.Logger.Warn("client misconfigured, no miner address specified - disconnecting", zap.String("client", client.String()))
					RecordWorkerError(client.WalletAddr, ErrNoMinerAddress)
					client.Disconnect() // invalid configuration, boot the worker
				}
				return
			}
			template, err := kapi.GetBlockTemplate(client)
			if err != nil {
				if strings.Contains(err.Error(), "Could not decode address") {
					RecordWorkerError(client.WalletAddr, ErrInvalidAddressFmt)
					client.Logger.Error(fmt.Sprintf("failed fetching new block template from kaspa, malformed address: %s", err))
					client.Disconnect() // unrecoverable
				} else {
					RecordWorkerError(client.WalletAddr, ErrFailedBlockFetch)
					client.Logger.Error(fmt.Sprintf("failed fetching new block template from kaspa: %s", err))
				}
				return
			}
			state.bigDiff = CalculateTarget(uint64(template.Block.Header.Bits))
			header, err := SerializeBlockHeader(template.Block)
			if err != nil {
				RecordWorkerError(client.WalletAddr, ErrBadDataFromMiner)
				client.Logger.Error(fmt.Sprintf("failed to serialize block header: %s", err))
				return
			}

			jobId := state.AddJob(template.Block)
			if !state.initialized {
				state.initialized = true
				state.useBigJob = bigJobRegex.MatchString(client.RemoteApp)
				// first pass through send config/default difficulty
				state.stratumDiff = newKaspaDiff()
				state.stratumDiff.setDiffValue(c.minShareDiff)
				sendClientDiff(client, state)
				c.shareHandler.setClientVardiff(client, c.minShareDiff)
			} else {
				varDiff := c.shareHandler.getClientVardiff(client)
				if varDiff != state.stratumDiff.diffValue && varDiff != 0 {
					// send updated vardiff
					client.Logger.Info(fmt.Sprintf("changing diff from %f to %f", state.stratumDiff.diffValue, varDiff))
					state.stratumDiff.setDiffValue(varDiff)
					sendClientDiff(client, state)
					c.shareHandler.startClientVardiff(client)
				}
			}

			jobParams := []any{fmt.Sprintf("%d", jobId)}
			if state.useBigJob {
				jobParams = append(jobParams, GenerateLargeJobParams(header, uint64(template.Block.Header.Timestamp)))
			} else {
				jobParams = append(jobParams, GenerateJobHeader(header))
				jobParams = append(jobParams, template.Block.Header.Timestamp)
			}

			// // normal notify flow
			if err := client.Send(gostratum.JsonRpcEvent{
				Version: "2.0",
				Method:  "mining.notify",
				Id:      jobId,
				Params:  jobParams,
			}); err != nil {
				if errors.Is(err, gostratum.ErrorDisconnected) {
					RecordWorkerError(client.WalletAddr, ErrDisconnected)
					return
				}
				RecordWorkerError(client.WalletAddr, ErrFailedSendWork)
				client.Logger.Error(errors.Wrapf(err, "failed sending work packet %d", jobId).Error())
			}

			RecordNewJob(client)
		}(cl)

		if cl.WalletAddr != "" {
			addresses = append(addresses, cl.WalletAddr)
		}
	}
	c.clientLock.Unlock()

	if time.Since(c.lastBalanceCheck) > balanceDelay {
		c.lastBalanceCheck = time.Now()
		if len(addresses) > 0 {
			go func() {
				balances, err := kapi.kaspad.GetBalancesByAddresses(addresses)
				if err != nil {
					c.logger.Warn("failed to get balances from kaspa, prom stats will be out of date", zap.Error(err))
					return
				}
				RecordBalances(balances)
			}()
		}
	}
}

func sendClientDiff(client *gostratum.StratumContext, state *MiningState) {
	if err := client.Send(gostratum.JsonRpcEvent{
		Version: "2.0",
		Method:  "mining.set_difficulty",
		Params:  []any{state.stratumDiff.diffValue},
	}); err != nil {
		RecordWorkerError(client.WalletAddr, ErrFailedSetDiff)
		client.Logger.Error(errors.Wrap(err, "failed sending difficulty").Error())
		return
	}
	client.Logger.Info(fmt.Sprintf("Setting client diff: %f", state.stratumDiff.diffValue))
}
