// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"

	//"os"
	"sync"
	"time"

	"github.com/magiconair/properties"
	"github.com/pingcap/go-ycsb/pkg/measurement"
	"github.com/pingcap/go-ycsb/pkg/prop"
	"github.com/pingcap/go-ycsb/pkg/ycsb"
	"github.com/pkg/errors"
)

type worker struct {
	p               *properties.Properties
	workDB          ycsb.DB
	workload        ycsb.Workload
	doTransactions  bool
	doBatch         bool
	batchSize       int
	opCount         int64
	targetOpsPerMs  float64
	threadID        int
	targetOpsTickNs int64
	opsDone         int64

	mean            float64
	std             float64
	delay           int64
	period          int
	noiseRatio      float64
}

func newWorker(p *properties.Properties, threadID int, threadCount int, workload ycsb.Workload, db ycsb.DB) *worker {
	w := new(worker)
	w.p = p
	w.doTransactions = p.GetBool(prop.DoTransactions, true)
	w.batchSize = p.GetInt(prop.BatchSize, prop.DefaultBatchSize)
	if w.batchSize > 1 {
		w.doBatch = true
	}
	w.threadID = threadID
	w.workload = workload
	w.workDB = db

	w.mean = w.p.GetFloat64(prop.ExpectedValue,prop.ExpectedValueDefault)
	w.std = w.p.GetFloat64(prop.StandardDeviation,prop.StandardDeviationDefault)
	w.delay = w.p.GetInt64(prop.TimeDelay,prop.TimeDelayDefault)
	w.period = w.p.GetInt(prop.TimePeriod,prop.TimePeriodDefault)
	w.noiseRatio = w.p.GetFloat64(prop.NoiseRatio,prop.NoiseRatioDefault)

	var totalOpCount int64
	if w.doTransactions {
		totalOpCount = p.GetInt64(prop.OperationCount, 0)
	} else {
		if _, ok := p.Get(prop.InsertCount); ok {
			totalOpCount = p.GetInt64(prop.InsertCount, 0)
		} else {
			totalOpCount = p.GetInt64(prop.RecordCount, 0)
		}
	}

	if totalOpCount < int64(threadCount) {
		fmt.Printf("totalOpCount(%s/%s/%s): %d should be bigger than threadCount: %d",
			prop.OperationCount,
			prop.InsertCount,
			prop.RecordCount,
			totalOpCount,
			threadCount)

		os.Exit(-1)
	}

	w.opCount = totalOpCount / int64(threadCount)

	targetPerThreadPerms := float64(-1)
	if v := p.GetInt64(prop.Target, 0); v > 0 {
		targetPerThread := float64(v) / float64(threadCount)
		targetPerThreadPerms = targetPerThread / 1000.0
	}

	if targetPerThreadPerms > 0 {
		w.targetOpsPerMs = targetPerThreadPerms
		w.targetOpsTickNs = int64(1000000.0 / w.targetOpsPerMs)
	}

	return w
}

func (w *worker) throttle(ctx context.Context, startTime time.Time) {
	if w.targetOpsPerMs <= 0 {
		return
	}

	d := time.Duration(w.opsDone * w.targetOpsTickNs)
	d = startTime.Add(d).Sub(time.Now())
	if d < 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (w *worker) ctlPeriod(loadStartTime time.Time) error {
	distributionName :=  w.p.GetString(prop.TimeDistribution,"none")
	switch distributionName {
	case "normal":
		timeNow := int(time.Now().Sub(loadStartTime).Minutes())%w.period
		v := 1 / (math.Sqrt(2*math.Pi) * w.std) * math.Pow(math.E, (-math.Pow((float64(timeNow) - w.mean), 2)/(2*math.Pow(w.std, 2))))
		tmp := 1/v*float64(w.delay)
		if tmp > 3 * math.Pow(10.0,10) {
			tmp = 3 * math.Pow(10.0,10)
		}
		time.Sleep(time.Duration(tmp))
	case "reverse_normal":
		timeNow := int(time.Now().Sub(loadStartTime).Minutes())%w.period
		v := 1 / (math.Sqrt(2*math.Pi) * w.std) * math.Pow(math.E, (-math.Pow((float64(timeNow) - w.mean), 2)/(2*math.Pow(w.std, 2))))
		tmp := v*float64(w.delay)
		if tmp > 3 * math.Pow(10.0,10) {
			tmp = 3 * math.Pow(10.0,10)
		}
		time.Sleep(time.Duration(tmp))
	case "noise_normal":
		timeNow := int(time.Now().Sub(loadStartTime).Minutes())%w.period
		v := 1 / (math.Sqrt(2*math.Pi) * w.std) * math.Pow(math.E, (-math.Pow((float64(timeNow) - w.mean), 2)/(2*math.Pow(w.std, 2))))
		tmp := 1/v*float64(w.delay)
		if tmp > 3 * math.Pow(10.0,10) {
			tmp = 3 * math.Pow(10.0,10)
		}
		getnoiseRange(timeNow,w.noiseRatio)
		noise := tmp*noiseRange
		time.Sleep(time.Duration(tmp+noise))
	case "step":
		timeNow := int(time.Now().Sub(loadStartTime).Minutes())%w.period
		v := 3 - int(0.5 + float64(timeNow * 3.0 /w.period))
		tmp := int64(v)*w.delay
		time.Sleep(time.Duration(tmp))
	case "noise_step":
		timeNow := int(time.Now().Sub(loadStartTime).Minutes())%w.period
		v := 3 - int(0.5 + float64(timeNow * 3.0 /w.period))
		tmp := float64(v)*float64(w.delay)
		getnoiseRange(timeNow,w.noiseRatio)
		noise := tmp*noiseRange
		time.Sleep(time.Duration(tmp+noise))
	default:
		fmt.Printf("distribtion_name err: ")
		return errors.Errorf("distribtion_name err: ")
	}
	return nil
}

var oldtime int = -1
var noiseRange float64 = 0
//if radio is y，noiserange is randfloat64 in [-y/(1+y),y/(1-y)]
func getnoiseRange(newtime int,ratio float64){
	//not locked, not necessary
	if newtime!=oldtime {
		if ratio == 1{
			//[-1/2,2]
			noiseRange = float64(rand.Int63n(5) -1)/2
		} else if ratio == 0{
			noiseRange = 0
		} else {
			//[-y/(1+y),y/(1-y)]
			noiseRange = float64(rand.Int63n(int64(10000*2*ratio)) -int64(10000*ratio*(1-ratio)))/(10000*(1-ratio*ratio))
		}
		oldtime = newtime
	}
}

func (w *worker) run(ctx context.Context,loadStartTime time.Time) {
	// spread the thread operation out so they don't all hit the DB at the same time
	if w.targetOpsPerMs > 0.0 && w.targetOpsPerMs <= 1.0 {
		time.Sleep(time.Duration(rand.Int63n(w.targetOpsTickNs)))
	}

	startTime := time.Now()

	for w.opCount == 0 || w.opsDone < w.opCount {
		var err error
		opsCount := 1
		if w.doTransactions {
			if w.doBatch {
				err = w.workload.DoBatchTransaction(ctx, w.batchSize, w.workDB)
				opsCount = w.batchSize
			} else {
				err = w.workload.DoTransaction(ctx, w.workDB)
			}
		} else {
			if w.doBatch {
				err = w.workload.DoBatchInsert(ctx, w.batchSize, w.workDB)
				opsCount = w.batchSize
			} else {
				err = w.workload.DoInsert(ctx, w.workDB)
			}
		}

		//Control delay makes data normal distribution in time dimension
		if w.p.GetBool(prop.NormalDataInTime, prop.NormalDataInTimeDefault) {
			err := w.ctlPeriod(loadStartTime)
			if err != nil {
				return
			}
		}

		if err != nil && !w.p.GetBool(prop.Silence, prop.SilenceDefault) {
			fmt.Printf("operation err: %v\n", err)
		}

		if measurement.IsWarmUpFinished() {
			w.opsDone += int64(opsCount)
			w.throttle(ctx, startTime)
		}

		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// Client is a struct which is used the run workload to a specific DB.
type Client struct {
	p        *properties.Properties
	workload ycsb.Workload
	db       ycsb.DB
}

// NewClient returns a client with the given workload and DB.
// The workload and db can't be nil.
func NewClient(p *properties.Properties, workload ycsb.Workload, db ycsb.DB) *Client {
	return &Client{p: p, workload: workload, db: db}
}

// Run runs the workload to the target DB, and blocks until all workers end.
func (c *Client) Run(ctx context.Context, loadStartTime time.Time) {
	var wg sync.WaitGroup
	threadCount := c.p.GetInt(prop.ThreadCount, 1)

	wg.Add(threadCount)
	measureCtx, measureCancel := context.WithCancel(ctx)
	measureCh := make(chan struct{}, 1)
	go func() {
		defer func() {
			measureCh <- struct{}{}
		}()
		// load stage no need to warm up
		if c.p.GetBool(prop.DoTransactions, true) {
			dur := c.p.GetInt64(prop.WarmUpTime, 0)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(dur) * time.Second):
			}
		}
		// finish warming up
		measurement.EnableWarmUp(false)

		dur := c.p.GetInt64("measurement.interval", 10)
		t := time.NewTicker(time.Duration(dur) * time.Second)
		defer t.Stop()

		for {
			select {
			case <-t.C:
				measurement.Output()
			case <-measureCtx.Done():
				return
			}
		}
	}()

	for i := 0; i < threadCount; i++ {
		go func(threadId int) {
			defer wg.Done()

			w := newWorker(c.p, threadId, threadCount, c.workload, c.db)
			ctx := c.workload.InitThread(ctx, threadId, threadCount)
			ctx = c.db.InitThread(ctx, threadId, threadCount)
			w.run(ctx,loadStartTime)
			c.db.CleanupThread(ctx)
			c.workload.CleanupThread(ctx)
		}(i)
	}

	wg.Wait()
	if !c.p.GetBool(prop.DoTransactions, true) {
		// when loading is finished, try to analyze table if possible.
		if analyzeDB, ok := c.db.(ycsb.AnalyzeDB); ok {
			analyzeDB.Analyze(ctx, c.p.GetString(prop.TableName, prop.TableNameDefault))
		}
	}
	measureCancel()
	<-measureCh
}
