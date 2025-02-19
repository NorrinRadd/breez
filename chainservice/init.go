package chainservice

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/breez/breez/config"
	"github.com/breez/breez/db"
	breezlog "github.com/breez/breez/log"
	"github.com/breez/breez/refcount"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btclog"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightninglabs/neutrino"
	"github.com/lightninglabs/neutrino/headerfs"
)

const (
	directoryPattern = "data/chain/bitcoin/{{network}}/"
)

var (
	serviceRefCounter refcount.ReferenceCountable
	service           *neutrino.ChainService
	walletDB          walletdb.DB
	logger            btclog.Logger
)

// Get returned a reusable ChainService
func Get(workingDir string, breezDB *db.DB) (cs *neutrino.ChainService, cleanupFn func() error, err error) {
	bootstrapMu.Lock()
	defer bootstrapMu.Unlock()

	chainSer, release, err := serviceRefCounter.Get(
		func() (interface{}, refcount.ReleaseFunc, error) {
			return createService(workingDir, breezDB)
		},
	)
	if err != nil {
		return nil, nil, err
	}
	service = chainSer.(*neutrino.ChainService)
	return service, release, err
}

func TestPeer(peer string) error {
	tempDir, err := ioutil.TempDir("", "testConnection")
	if err != nil {
		logger.Errorf("Error in ioutil.TempDir: %v", err)
		return err
	}
	defer os.RemoveAll(tempDir)
	logger.Infof("TestDir tempDir: %v", tempDir)

	neutrinoDataDir := path.Join(tempDir, "data")
	if err := os.MkdirAll(neutrinoDataDir, 0700); err != nil {
		logger.Errorf("Error in os.MkdirAll %v", err)
		return err
	}
	neutrinoDB := path.Join(neutrinoDataDir, "neutrino.db")
	db, err := walletdb.Create("bdb", neutrinoDB, false, time.Second*60)
	if err != nil {
		logger.Errorf("Error in walletdb.Create: %v", err)
		return err
	}

	neutrinoConfig := neutrino.Config{
		DataDir:      neutrinoDataDir,
		Database:     db,
		ChainParams:  chaincfg.MainNetParams,
		ConnectPeers: []string{peer},
	}
	chainService, err := neutrino.NewChainService(neutrinoConfig)
	if err != nil {
		logger.Errorf("Error in neutrino.NewChainService: %v", err)
		return err
	}
	err = chainService.Start()
	if err != nil {
		logger.Errorf("Error in chainService.Start: %v", err)
		return err
	}
	time.Sleep(10 * time.Second)
	c := chainService.ConnectedCount()
	if c < 1 {
		logger.Errorf("chainService.ConnectedCount() returned 0")
		return fmt.Errorf("Cannot connect to peer")
	}
	return nil
}

func createService(workingDir string, breezDB *db.DB) (*neutrino.ChainService, refcount.ReleaseFunc, error) {
	var err error
	neutrino.MaxPeers = 1
	neutrino.BanDuration = 5 * time.Second
	neutrino.ConnectionRetryInterval = 1 * time.Second
	config, err := config.GetConfig(workingDir)
	if err != nil {
		return nil, nil, err
	}
	if logger == nil {
		logger, err = breezlog.GetLogger(workingDir, "CHAIN")
		if err != nil {
			return nil, nil, err
		}
		logger.Infof("After get logger")
		logger.SetLevel(btclog.LevelDebug)
		neutrino.UseLogger(logger)
	}
	logger.Infof("creating shared chain service.")

	peers, _, err := breezDB.GetPeers(config.JobCfg.ConnectedPeers)
	if err != nil {
		logger.Errorf("peers error: %v", err)
		return nil, nil, err
	}

	service, walletDB, err = newNeutrino(workingDir, config, peers)
	if err != nil {
		logger.Errorf("failed to create chain service %v", err)
		return nil, stopService, err
	}

	logger.Infof("chain service was created successfuly")
	return service, stopService, err
}

func stopService() error {
	if service != nil && service.IsStarted() {
		if err := service.Stop(); err != nil {
			return err
		}
		service = nil
	}
	if walletDB != nil {
		if err := walletDB.Close(); err != nil {
			return err
		}
	}
	return nil
}

func chainParams(network string) (*chaincfg.Params, error) {
	var params *chaincfg.Params
	switch network {
	case "testnet":
		params = &chaincfg.TestNet3Params
	case "simnet":
		params = &chaincfg.SimNetParams
	case "mainnet":
		params = &chaincfg.MainNetParams
	}

	if params == nil {
		return nil, fmt.Errorf("Unrecognized network %v", network)
	}
	return params, nil
}

func neutrinoDataDir(workingDir string, network string) string {
	dataPath := strings.Replace(directoryPattern, "{{network}}", network, -1)
	return path.Join(workingDir, dataPath)
}

func parseAssertFilterHeader(headerStr string) (*headerfs.FilterHeader, error) {
	if headerStr == "" {
		return nil, nil
	}

	heightAndHash := strings.Split(headerStr, ":")

	height, err := strconv.ParseUint(heightAndHash[0], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid filter header height: %v", err)
	}

	hash, err := chainhash.NewHashFromStr(heightAndHash[1])
	if err != nil {
		return nil, fmt.Errorf("invalid filter header hash: %v", err)
	}

	return &headerfs.FilterHeader{
		FilterHash: *hash,
		Height:     uint32(height),
	}, nil
}

/*
newNeutrino creates a chain service that the sync job uses
in order to fetch chain data such as headers, filters, etc...
*/
func newNeutrino(workingDir string, cfg *config.Config, peers []string) (*neutrino.ChainService, walletdb.DB, error) {
	params, err := chainParams(cfg.Network)

	if err != nil {
		return nil, nil, err
	}

	ensureNeutrinoSize(workingDir)

	neutrinoDataDir, db, err := getNeutrinoDB(workingDir)
	if err != nil {
		return nil, nil, err
	}
	neutrinoConfig := neutrino.Config{
		DataDir:      neutrinoDataDir,
		Database:     db,
		ChainParams:  *params,
		ConnectPeers: peers,
	}
	logger.Infof("creating new neutrino service.")
	chainService, err := neutrino.NewChainService(neutrinoConfig)
	return chainService, db, err
}

func getNeutrinoDB(workingDir string) (string, walletdb.DB, error) {
	config, err := config.GetConfig(workingDir)
	if err != nil {
		return "", nil, err
	}
	neutrinoDataDir := neutrinoDataDir(workingDir, config.Network)
	neutrinoDB := path.Join(neutrinoDataDir, "neutrino.db")
	if err := os.MkdirAll(neutrinoDataDir, 0700); err != nil {
		return "", nil, err
	}

	db, err := walletdb.Create("bdb", neutrinoDB, false, time.Second*60)
	return neutrinoDataDir, db, err
}

func ensureNeutrinoSize(workingDir string) error {
	config, err := config.GetConfig(workingDir)
	if err != nil {
		return err
	}
	neutrinoDataDir := neutrinoDataDir(workingDir, config.Network)
	neutrinoDB := path.Join(neutrinoDataDir, "neutrino.db")
	if err := purgeOversizeFilters(neutrinoDB); err != nil {
		logger.Errorf("failed to purgeOversizeFilters %v, moving to reset chain service", err)
		if err := resetChainService(workingDir); err != nil {
			logger.Errorf("failed to reset chain service %v", err)
			return err
		}
	}
	return nil
}

func resetChainService(workingDir string) error {
	config, err := config.GetConfig(workingDir)
	if err != nil {
		return err
	}
	neutrinoDataDir := neutrinoDataDir(workingDir, config.Network)
	if err = os.Remove(path.Join(neutrinoDataDir, "neutrino.db")); err != nil {
		logger.Errorf("failed to remove neutrino.db %v", err)
	}
	if err = os.Remove(path.Join(neutrinoDataDir, "reg_filter_headers.bin")); err != nil {
		logger.Errorf("failed to remove reg_filter_headers.bin %v", err)
	}
	if err = os.Remove(path.Join(neutrinoDataDir, "block_headers.bin")); err != nil {
		logger.Errorf("failed to remove block_headers.bin %v", err)
	}

	return nil
}
