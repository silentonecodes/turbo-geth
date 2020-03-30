package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/pprof"
	"syscall"

	"github.com/ledgerwatch/turbo-geth/cmd/utils"
	"github.com/ledgerwatch/turbo-geth/core"
	"github.com/ledgerwatch/turbo-geth/log"
	"github.com/spf13/cobra"
)

var (
	cpuprofile     string
	cpuProfileFile io.WriteCloser
	genesisPath    string
	genesis        *core.Genesis
)

func init() {
	rootCmd.PersistentFlags().StringVar(&cpuprofile, "cpuprofile", "", "write cpu profile `file`")
	rootCmd.PersistentFlags().StringVar(&genesisPath, "genesis", "", "path to genesis.json file")
}

func rootContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(ch)

		select {
		case <-ch:
			log.Info("Got interrupt, shutting down...")
		case <-ctx.Done():
		}

		cancel()
	}()
	return ctx
}

var rootCmd = &cobra.Command{
	Use:   "state",
	Short: "state is a utility for Stateless ethereum clients",
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		genesis = core.DefaultGenesisBlock()
		if genesisPath != "" {
			genesis = genesisFromFile(genesisPath)
		}

		startProfilingIfNeeded()
	},
	PersistentPostRun: func(cmd *cobra.Command, args []string) {
		stopProfilingIfNeeded()
	},
}

func genesisFromFile(genesisPath string) *core.Genesis {
	file, err := os.Open(genesisPath)
	if err != nil {
		utils.Fatalf("Failed to read genesis file: %v", err)
	}
	defer file.Close()

	genesis := new(core.Genesis)
	if err := json.NewDecoder(file).Decode(genesis); err != nil {
		utils.Fatalf("invalid genesis file: %v", err)
	}
	return genesis
}

func Execute() {
	if err := rootCmd.ExecuteContext(rootContext()); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func startProfilingIfNeeded() {
	if cpuprofile != "" {
		fmt.Println("starting CPU profiling")
		cpuProfileFile, err := os.Create(cpuprofile)
		if err != nil {
			log.Error("could not create CPU profile", "error", err)
			return
		}
		if err := pprof.StartCPUProfile(cpuProfileFile); err != nil {
			log.Error("could not start CPU profile", "error", err)
			return
		}
	}
}

func stopProfilingIfNeeded() {
	if cpuprofile != "" {
		fmt.Println("stopping CPU profiling")
		pprof.StopCPUProfile()
	}

	if cpuProfileFile != nil {
		cpuProfileFile.Close()
	}
}
