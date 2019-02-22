// Copyright (c) 2019 IoTeX
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package committee

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"math/big"
	"sync"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/pkg/errors"

	"github.com/iotexproject/iotex-election/carrier"
	"github.com/iotexproject/iotex-election/types"
	"github.com/iotexproject/iotex-election/util"
	"github.com/iotexproject/iotex-election/db"
)


// CalcBeaconChainHeight calculates the corresponding beacon chain height for an epoch
type CalcBeaconChainHeight func(uint64) (uint64, error)

// Config defines the config of the committee
type Config struct {
	NumOfRetries              uint8  `yaml:"numOfRetries"`
	BeaconChainAPI            string `yaml:"beaconChainAPI"`
	BeaconChainHeightInterval uint64 `yaml:"beaconChainHeightInterval"`
	BeaconChainStartHeight    uint64 `yaml:"beaconChainStartHeight"`
	RegisterContractAddress   string `yaml:"registerContractAddress"`
	StakingContractAddress    string `yaml:"stakingContractAddress"`
	PaginationSize            uint8  `yaml:"paginationSize"`
	VoteThreshold             string `yaml:"voteThreshold"`
	ScoreThreshold            string `yaml:"scoreThreshold"`
	SelfStakingThreshold      string `yaml:"selfStakingThreshold"`
	CacheSize                 uint8  `yaml:"cacheSize"`
}

// Committee defines an interface of an election committee
// It could be considered as a light state db of beacon chain, that
type Committee interface {
	// Start starts the committee service
	Start(context.Context) error
	// Stop stops the committee service
	Stop(context.Context) error
	// ResultByHeight returns the result on a specific ethereum height
	ResultByHeight(height uint64) (*types.ElectionResult, error)
	// HeightByTime returns the nearest result before time
	HeightByTime(timestamp time.Time) (uint64, error)
	// OnNewBlock is a callback function which will be called on new block created
	OnNewBlock(height uint64)
	// LatestHeight returns the height with latest result
	LatestHeight() uint64
}

type committee struct {
	db                   db.KVStore
	carrier              carrier.Carrier
	retryLimit           uint8
	paginationSize       uint8
	voteThreshold        *big.Int
	scoreThreshold       *big.Int
	selfStakingThreshold *big.Int
	interval             uint64
	cache                *resultCache
	heightManager        *heightManager
	startHeight          uint64
	nextHeight           uint64
	currentHeight        uint64
	terminate            chan bool
	mutex                sync.RWMutex
}

// NewCommittee creates a committee
func NewCommittee(db db.KVStore, cfg Config) (Committee, error) {
	if !common.IsHexAddress(cfg.StakingContractAddress) {
		return nil, errors.New("Invalid staking contract address")
	}
	carrier, err := carrier.NewEthereumVoteCarrier(
		cfg.BeaconChainAPI,
		common.HexToAddress(cfg.RegisterContractAddress),
		common.HexToAddress(cfg.StakingContractAddress),
	)
	if err != nil {
		return nil, err
	}
	voteThreshold, ok := new(big.Int).SetString(cfg.VoteThreshold, 10)
	if !ok {
		return nil, errors.New("Invalid vote threshold")
	}
	scoreThreshold, ok := new(big.Int).SetString(cfg.ScoreThreshold, 10)
	if !ok {
		return nil, errors.New("Invalid score threshold")
	}
	selfStakingThreshold, ok := new(big.Int).SetString(cfg.SelfStakingThreshold, 10)
	if !ok {
		return nil, errors.New("Invalid self staking threshold")
	}
	return &committee{
		db:                   db,
		cache:                newResultCache(cfg.CacheSize),
		heightManager:        newHeightManager(),
		carrier:              carrier,
		retryLimit:           cfg.NumOfRetries,
		paginationSize:       cfg.PaginationSize,
		voteThreshold:        voteThreshold,
		scoreThreshold:       scoreThreshold,
		selfStakingThreshold: selfStakingThreshold,
		terminate:            make(chan bool),
		startHeight:          cfg.BeaconChainStartHeight,
		interval:             cfg.BeaconChainHeightInterval,
		currentHeight:        0,
		nextHeight:           cfg.BeaconChainStartHeight,
	}, nil
}

func (ec *committee) Start(ctx context.Context) (err error) {
	ec.mutex.Lock()
	defer ec.mutex.Unlock()
	if err := ec.db.Start(ctx); err != nil {
		return errors.Wrap(err, "error when starting db")
	}
	if startHeight, err := ec.db.Get(db.NextHeightKey); err == nil {
		ec.nextHeight = util.BytesToUint64(startHeight)
	}
	for {
		result, err := ec.fetchResultByHeight(ec.nextHeight)
		if err == db.ErrNotExist {
			break
		}
		fmt.Println("height", ec.nextHeight)
		for _, d := range result.Delegates() {
			fmt.Println("delegate", hex.EncodeToString(d.Name()), d.Score())
		}
		if err == nil {
			if err := ec.heightManager.add(ec.nextHeight, result.MintTime()); err != nil {
				return err
			}
			if err = ec.storeResult(ec.nextHeight, result); err != nil {
				return err
			}
			ec.cache.insert(ec.nextHeight, result)
			ec.currentHeight = ec.nextHeight
			ec.nextHeight += ec.interval
			continue
		}
		return err
	}
	for i := uint8(0); i < ec.retryLimit; i++ {
		if err = ec.carrier.SubscribeNewBlock(ec.OnNewBlock, ec.terminate); err == nil {
			break
		}
		fmt.Println("retry new block subscription")
	}
	return
}

func (ec *committee) Stop(ctx context.Context) error {
	ec.mutex.Lock()
	defer ec.mutex.Unlock()
	ec.terminate <- true
	return nil
}

func (ec *committee) OnNewBlock(tipHeight uint64) {
	ec.mutex.Lock()
	defer ec.mutex.Unlock()
	if ec.currentHeight < tipHeight {
		ec.currentHeight = tipHeight
	}
	for {
		if ec.nextHeight > ec.currentHeight {
			break
		}
		var result *types.ElectionResult
		var err error
		for i := uint8(0); i < ec.retryLimit; i++ {
			if result, err = ec.fetchResultByHeight(ec.nextHeight); err != nil {
				log.Println(err)
				continue
			}
			break
		}
		if result == nil {
			log.Printf("failed to fetch result for %d\n", ec.nextHeight)
			return
		}
		if err = ec.heightManager.validate(ec.nextHeight, result.MintTime()); err != nil {
			log.Fatalln(
				"Unexpected status that the upcoming block height or time is invalid",
				err,
			)
			return
		}
		if err = ec.storeResult(ec.nextHeight, result); err != nil {
			log.Println("failed to store result into db", err)
			return
		}
		ec.heightManager.add(ec.nextHeight, result.MintTime())
		ec.cache.insert(ec.nextHeight, result)
		ec.nextHeight += ec.interval
	}
}

func (ec *committee) LatestHeight() uint64 {
	ec.mutex.RLock()
	defer ec.mutex.RUnlock()
	return ec.heightManager.lastestHeight()
}

func (ec *committee) HeightByTime(ts time.Time) (uint64, error) {
	ec.mutex.RLock()
	defer ec.mutex.RUnlock()
	height := ec.heightManager.nearestHeightBefore(ts)
	if height == 0 {
		return 0, db.ErrNotExist
	}

	return height, nil
}

func (ec *committee) ResultByHeight(height uint64) (*types.ElectionResult, error) {
	ec.mutex.RLock()
	defer ec.mutex.RUnlock()
	return ec.resultByHeight(height)
}

func (ec *committee) resultByHeight(height uint64) (*types.ElectionResult, error) {
	if height < ec.startHeight {
		return nil, errors.Errorf(
			"height %d is higher than start height %d",
			height,
			ec.startHeight,
		)
	}
	if (height-ec.startHeight)%ec.interval != 0 {
		return nil, errors.Errorf(
			"height %d is an invalid height",
			height,
		)
	}
	result := ec.cache.get(height)
	if result != nil {
		return result, nil
	}
	data, err := ec.db.Get(ec.dbKey(height))
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, db.ErrNotExist
	}
	result = &types.ElectionResult{}

	return result, result.Deserialize(data)
}

func (ec *committee) calcWeightedVotes(v *types.Vote, now time.Time) *big.Int {
	if now.Before(v.StartTime()) {
		return big.NewInt(0)
	}
	remainingTime := v.RemainingTime(now).Seconds()
	weight := float64(1)
	if remainingTime > 0 {
		weight += math.Log(math.Ceil(remainingTime/86400)) / math.Log(1.2)
	}
	amount := new(big.Float).SetInt(v.Amount())
	weightedAmount, _ := amount.Mul(amount, big.NewFloat(weight)).Int(nil)

	return weightedAmount
}

func (ec *committee) fetchResultByHeight(height uint64) (*types.ElectionResult, error) {
	fmt.Printf("fetch result for %d\n", height)
	mintTime, err := ec.carrier.BlockTimestamp(height)
	switch errors.Cause(err) {
	case nil:
		break
	case ethereum.NotFound:
		return nil, db.ErrNotExist
	default:
		return nil, err
	}
	calculator := types.NewResultCalculator(
		mintTime,
		func(v *types.Vote) bool {
			return ec.voteThreshold.Cmp(v.Amount()) > 0
		},
		ec.calcWeightedVotes,
		func(c *types.Candidate) bool {
			return ec.selfStakingThreshold.Cmp(c.SelfStakingScore()) > 0 &&
				ec.scoreThreshold.Cmp(c.Score()) > 0
		},
	)
	previousIndex := big.NewInt(1)
	for {
		var candidates []*types.Candidate
		var err error
		if previousIndex, candidates, err = ec.carrier.Candidates(
			height,
			previousIndex,
			ec.paginationSize,
		); err != nil {
			return nil, err
		}
		calculator.AddCandidates(candidates)
		if len(candidates) < int(ec.paginationSize) {
			break
		}
	}
	previousIndex = big.NewInt(0)
	for {
		var votes []*types.Vote
		var err error
		if previousIndex, votes, err = ec.carrier.Votes(
			height,
			previousIndex,
			ec.paginationSize,
		); err != nil {
			return nil, err
		}
		calculator.AddVotes(votes)
		if len(votes) < int(ec.paginationSize) {
			break
		}
	}
	return calculator.Calculate()
}

func (ec *committee) dbKey(height uint64) []byte {
	return util.Uint64ToBytes(height)
}

func (ec *committee) storeResult(height uint64, result *types.ElectionResult) error {
	data, err := result.Serialize()
	if err != nil {
		return err
	}

	if err := ec.db.Put(ec.dbKey(height), data); err != nil {
		return errors.Wrapf(err, "failed to put election result into db")
	}

	return ec.db.Put(db.NextHeightKey, ec.dbKey(height+ec.interval))
}
