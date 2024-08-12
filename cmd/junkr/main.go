package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	rhp2 "go.sia.tech/core/rhp/v2"
	"go.sia.tech/renterd/api"
	"go.sia.tech/renterd/worker"
	"go.uber.org/zap"
	"lukechampine.com/frand"
)

var (
	workerAddr = "localhost:9980/api/worker"
	workerPass = ""

	workers int   = 10
	sectors int64 = 90 // ~ 1 GiB after erasure coding

	logLevel string
	logPath  string

	timeMu      sync.Mutex
	uploadTimes []time.Duration
)

func init() {
	flag.StringVar(&logLevel, "log.level", "info", "the log level to use")
	flag.StringVar(&logPath, "log.path", "", "the path to write the log to")

	flag.StringVar(&workerAddr, "worker.addr", workerAddr, "the address of the renterd worker API")
	flag.StringVar(&workerPass, "worker.pass", workerPass, "the password of the renterd worker API")

	flag.IntVar(&workers, "workers", workers, "the number of workers to use")
	flag.Int64Var(&sectors, "sectors", sectors, "the number of sectors to upload")
	flag.Parse()
}

func formatBpsString(b int64, t time.Duration) string {
	const units = "KMGTPE"
	const factor = 1000

	time := t.Truncate(time.Second).Seconds()
	if time <= 0 {
		return "0.00 bps"
	}

	// calculate bps
	speed := float64(b*8) / time

	// short-circuit for < 1000 bits/s
	if speed < factor {
		return fmt.Sprintf("%.2f bps", speed)
	}

	var i = -1
	for ; speed >= factor; i++ {
		speed /= factor
	}
	return fmt.Sprintf("%.2f %cbps", speed, units[i])
}

func uploadWorker(ctx context.Context, n int, log *zap.Logger) {
	workerClient := worker.NewClient(workerAddr, workerPass)

	uploadSize := sectors * rhp2.SectorSize
	encodedSize := uploadSize * 3
	log = log.With(zap.Int("worker", n))
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		start := time.Now()

		filepath := fmt.Sprintf("junk-%s", hex.EncodeToString(frand.Bytes(16)))
		log.Debug("starting upload", zap.String("filepath", filepath), zap.Int64("encodedBytes", encodedSize))
		r := io.LimitReader(frand.Reader, uploadSize)
		if _, err := workerClient.UploadObject(ctx, r, "junk", filepath, api.UploadObjectOptions{}); err != nil {
			log.Error("upload failed", zap.Error(err), zap.Duration("elapsed", time.Since(start)))
		} else {
			log.Info("upload complete", zap.Int64("encodedBytes", encodedSize), zap.Duration("elapsed", time.Since(start)), zap.String("speed", formatBpsString(encodedSize, time.Since(start))))
			timeMu.Lock()
			uploadTimes = append(uploadTimes, time.Since(start))
			timeMu.Unlock()
		}
	}
}

func main() {
	cfg := zap.NewProductionConfig()
	switch logLevel {
	case "debug":
		cfg.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	case "info":
		cfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	case "warn":
		cfg.Level = zap.NewAtomicLevelAt(zap.WarnLevel)
	default:
		cfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	}
	cfg.OutputPaths = []string{"stdout"}
	if len(logPath) > 0 {
		cfg.OutputPaths = append(cfg.OutputPaths, logPath)
	}

	log, err := cfg.Build()
	if err != nil {
		panic(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		t := time.NewTicker(2 * time.Minute)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				timeMu.Lock()
				if len(uploadTimes) > 1000 {
					uploadTimes = uploadTimes[len(uploadTimes)-1000:]
				}
				times := uploadTimes
				timeMu.Unlock()
				var avg time.Duration
				for _, t := range times {
					avg += t
				}
				avg /= time.Duration(len(times))
				size := sectors * rhp2.SectorSize * 3 * int64(workers) // use the size after erasure coding and multiply by the number of workers to estimate the total upload speed
				log.Info("average upload time", zap.String("averageSpeed", formatBpsString(size, avg)))
			}
		}
	}()

	for n := 1; n <= workers; n++ {
		go uploadWorker(ctx, n, log)
	}

	<-ctx.Done()
	log.Info("shutting down...")
	time.Sleep(10 * time.Second)
}
