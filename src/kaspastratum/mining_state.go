package kaspastratum

import (
	"math/big"
	"sync"
	"time"

	"github.com/kaspanet/kaspad/app/appmessage"
	"github.com/onemorebsmith/kaspastratum/src/gostratum"
)

const maxjobs = 32

type MiningState struct {
	Jobs            map[uint64]*appmessage.RPCBlock
	SubmittedNonces map[uint64]map[string]bool
	JobLock         sync.Mutex
	jobCounter      uint64
	bigDiff         big.Int
	initialized     bool
	useBigJob       bool
	connectTime     time.Time
	stratumDiff     *kaspaDiff
	maxJobs         uint8
}

func MiningStateGenerator() any {
	return &MiningState{
		Jobs:            make(map[uint64]*appmessage.RPCBlock, maxjobs),
		SubmittedNonces: make(map[uint64]map[string]bool),
		JobLock:         sync.Mutex{},
		connectTime:     time.Now(),
		maxJobs:         maxjobs,
	}
}

func GetMiningState(ctx *gostratum.StratumContext) *MiningState {
	return ctx.State.(*MiningState)
}

func (ms *MiningState) AddJob(job *appmessage.RPCBlock) uint64 {
	ms.JobLock.Lock()
	defer ms.JobLock.Unlock()
	ms.jobCounter++
	idx := ms.jobCounter
	jobKey := idx % maxjobs
	if _, ok := ms.Jobs[jobKey]; ok {
		// job is being replaced, clear submitted nonces
		delete(ms.SubmittedNonces, jobKey)
	}
	ms.Jobs[jobKey] = job
	return idx
}

func (ms *MiningState) GetJob(id uint64) (*appmessage.RPCBlock, bool) {
	ms.JobLock.Lock()
	defer ms.JobLock.Unlock()
	job, exists := ms.Jobs[id%maxjobs]
	return job, exists
}

func (ms *MiningState) AddNonce(jobId uint64, nonce string) bool {
	ms.JobLock.Lock()
	defer ms.JobLock.Unlock()
	jobKey := jobId % maxjobs
	if _, ok := ms.Jobs[jobKey]; !ok {
		// job doesn't exist. stale?
		return false
	}

	if _, ok := ms.SubmittedNonces[jobKey]; !ok {
		ms.SubmittedNonces[jobKey] = make(map[string]bool)
	}

	if _, ok := ms.SubmittedNonces[jobKey][nonce]; ok {
		return false // duplicate
	}

	ms.SubmittedNonces[jobKey][nonce] = true
	return true
}
