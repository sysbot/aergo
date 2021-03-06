/**
 *  @file
 *  @copyright defined in aergo/LICENSE.txt
 */
package main

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"time"

	"github.com/aergoio/aergo-lib/log"
	"github.com/aergoio/aergo/account"
	"github.com/aergoio/aergo/blockchain"
	"github.com/aergoio/aergo/config"
	"github.com/aergoio/aergo/consensus"
	"github.com/aergoio/aergo/consensus/impl"
	"github.com/aergoio/aergo/internal/common"
	"github.com/aergoio/aergo/mempool"
	"github.com/aergoio/aergo/p2p"
	"github.com/aergoio/aergo/pkg/component"
	rest "github.com/aergoio/aergo/rest"
	"github.com/aergoio/aergo/rpc"
	"github.com/spf13/cobra"
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		panic(err)
	}
}

var (
	rootCmd = &cobra.Command{
		Use:   "aergosvr",
		Short: "Aergo Server",
		Long:  "Aergo Server Full-node implementation",
		Run:   rootRun,
	}
	homePath       string
	configFilePath string
	svrlog         *log.Logger

	cfg *config.Config
)

func init() {
	cobra.OnInitialize(initConfig)
	fs := rootCmd.PersistentFlags()
	fs.StringVar(&homePath, "home", "", "path of aergo home")
	fs.StringVar(&configFilePath, "config", "", "path of configuration file")
}

func initConfig() {
	serverCtx := config.NewServerContext(homePath, configFilePath)
	cfg = serverCtx.GetDefaultConfig().(*config.Config)
	err := serverCtx.LoadOrCreateConfig(cfg)
	if err != nil {
		fmt.Printf("Fail to load configuration file %v: %v", serverCtx.Vc.ConfigFileUsed(), err.Error())
		os.Exit(1)
	}
}

func rootRun(cmd *cobra.Command, args []string) {

	svrlog = log.NewLogger("asvr")
	svrlog.Info().Msg("AERGO SVR STARTED")

	if cfg.EnableProfile {
		svrlog.Info().Msgf("Enable Profiling on localhost:", cfg.ProfilePort)
		go func() {
			err := http.ListenAndServe(fmt.Sprintf("0.0.0.0:%d", cfg.ProfilePort), nil)
			svrlog.Info().Err(err).Msg("Run Profile Server")
		}()
	}

	compMng := component.NewComponentHub()
	chainsvc := blockchain.NewChainService(cfg)
	compMng.Register(chainsvc)
	mpoolsvc := mempool.NewMemPoolService(cfg)
	compMng.Register(mpoolsvc)
	accountsvc := account.NewAccountService(cfg)
	compMng.Register(accountsvc)
	rpcsvc := rpc.NewRPC(compMng, cfg)
	compMng.Register(rpcsvc)
	p2psvc := p2p.NewP2P(compMng, cfg, chainsvc)
	compMng.Register(p2psvc)

	if cfg.EnableRest {
		svrlog.Info().Msg("Start Rest server")
		restsvc := rest.NewRestService(cfg, chainsvc)
		compMng.Register(restsvc)
		//restsvc.Start()
	} else {
		svrlog.Info().Msg("Do not Start Rest server")
	}

	compMng.Start()

	c, err := impl.New(cfg, compMng)
	if err != nil {
		svrlog.Error().Err(err).Msg("failed to start consensus service. server shutdown")
		os.Exit(1)
	}
	if cfg.Consensus.EnableBp {
		consensus.Start(c)
	}
	chainsvc.SendChainInfo(c)

	common.HandleKillSig(func() {
		consensus.Stop(c)
		compMng.Stop()
	}, svrlog)

	// wait... TODO need to break out when system finished.
	for {
		time.Sleep(time.Minute)
	}
}
