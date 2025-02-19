package lnnode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sync/atomic"
	"time"

	"github.com/breez/breez/chainservice"
	"github.com/breez/breez/channeldbservice"
	breezlog "github.com/breez/breez/log"
	"github.com/dustin/go-humanize"
	"github.com/jessevdk/go-flags"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/breezbackuprpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/submarineswaprpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/signal"
)

const (
	activeGraceDuration = time.Second * 15
)

// Start is used to start the lightning network daemon.
func (d *Daemon) Start() error {
	if atomic.SwapInt32(&d.started, 1) == 1 {
		return errors.New("Daemon already started")
	}
	d.startTime = time.Now()

	if err := d.ntfnServer.Start(); err != nil {
		return err
	}

	checkMacaroons(d.cfg)
	if err := d.startDaemon(); err != nil {
		return fmt.Errorf("Failed to start daemon: %v", err)
	}

	return nil
}

// HasActiveChannel returns true if the node has at least one active channel.
func (d *Daemon) HasActiveChannel() bool {
	lnclient := d.APIClient()
	if lnclient == nil {
		return false
	}
	channels, err := lnclient.ListChannels(context.Background(), &lnrpc.ListChannelsRequest{
		ActiveOnly: true,
	})
	if err != nil {
		d.log.Errorf("Error in HasActiveChannel() > ListChannels(): %v", err)
		return false
	}
	return len(channels.Channels) > 0
}

// WaitReadyForPayment is waiting untill we are ready to pay
func (d *Daemon) WaitReadyForPayment(timeout time.Duration) error {
	client, err := d.ntfnServer.Subscribe()
	if err != nil {
		return err
	}
	defer client.Cancel()

	if d.IsReadyForPayment() {
		return nil
	}

	d.log.Infof("WaitReadyForPayment - not yet ready for payment, waiting...")
	timeoutTimer := time.After(timeout)
	for {
		select {
		case event := <-client.Updates():
			switch event.(type) {
			case ChannelEvent:
				d.log.Infof("WaitReadyForPayment got channel event %v", d.IsReadyForPayment())
				if d.IsReadyForPayment() {
					return nil
				}
			}
		case <-timeoutTimer:
			if d.IsReadyForPayment() {
				return nil
			}
			d.log.Info("WaitReadyForPayment got timeout event")
			return fmt.Errorf("timeout has exceeded while trying to process your request")
		}
	}
}

// IsReadyForPayment returns true if we can pay
func (d *Daemon) IsReadyForPayment() bool {
	lnclient := d.APIClient()
	if lnclient == nil {
		return false
	}
	allChannelsActive, err := d.allChannelsActive(lnclient)
	if err != nil {
		d.log.Errorf("Error in allChannelsActive(): %v", err)
		return false
	}
	return allChannelsActive
}

// NodePubkey returns the identity public key of the lightning node.
func (d *Daemon) NodePubkey() string {
	d.Lock()
	defer d.Unlock()
	return d.nodePubkey
}

// Stop is used to stop the lightning network daemon.
func (d *Daemon) Stop() error {
	if atomic.SwapInt32(&d.stopped, 1) == 0 {
		d.stopDaemon()
		d.ntfnServer.Stop()
	}
	d.wg.Wait()
	d.log.Infof("Daemon shutdown successfully")
	return nil
}

// APIClient returns the interface to query the daemon.
func (d *Daemon) APIClient() lnrpc.LightningClient {
	d.Lock()
	defer d.Unlock()
	return d.lightningClient
}

func (d *Daemon) SubSwapClient() submarineswaprpc.SubmarineSwapperClient {
	d.Lock()
	defer d.Unlock()
	return d.subswapClient
}

func (d *Daemon) BreezBackupClient() breezbackuprpc.BreezBackuperClient {
	d.Lock()
	defer d.Unlock()
	return d.breezBackupClient
}

func (d *Daemon) RouterClient() routerrpc.RouterClient {
	d.Lock()
	defer d.Unlock()
	return d.routerClient
}

func (d *Daemon) WalletKitClient() walletrpc.WalletKitClient {
	d.Lock()
	defer d.Unlock()
	return d.walletKitClient
}

func (d *Daemon) ChainNotifierClient() chainrpc.ChainNotifierClient {
	d.Lock()
	defer d.Unlock()
	return d.chainNotifierClient
}

func (d *Daemon) SignerClient() signrpc.SignerClient {
	d.Lock()
	defer d.Unlock()
	return d.signerClient
}

func (d *Daemon) InvoicesClient() invoicesrpc.InvoicesClient {
	d.Lock()
	defer d.Unlock()
	return d.invoicesClient
}

// RestartDaemon is used to restart a daemon that from some reason failed to start
// or was started and failed at some later point.
func (d *Daemon) RestartDaemon() error {
	if atomic.LoadInt32(&d.started) == 0 {
		return errors.New("Daemon must be started before attempt to restart")
	}
	return d.startDaemon()
}

func (d *Daemon) du(currentPath string, info os.FileInfo) int64 {
	size := info.Size()
	if !info.IsDir() {
		d.log.Errorf("%v: %v", info.Name(), humanize.Bytes(uint64(size)))
		return size
	}

	dir, err := os.Open(currentPath)
	if err != nil {
		d.log.Errorf("os.Open(%v) error: %v", currentPath, err)
		return size
	}
	defer dir.Close()

	fis, err := dir.Readdir(-1)
	if err != nil {
		d.log.Errorf("dir.Readdir(-1) error: %v", err)
		return 0
	}
	for _, fi := range fis {
		if fi.Name() == "." || fi.Name() == ".." {
			continue
		}
		size += d.du(currentPath+"/"+fi.Name(), fi)
	}
	d.log.Errorf("%v: %v", currentPath, humanize.Bytes(uint64(size)))
	return size
}

func (d *Daemon) startDaemon() error {
	d.Lock()
	defer d.Unlock()
	if d.daemonRunning {
		return errors.New("Daemon already running")
	}

	d.quitChan = make(chan struct{})
	readyChan := make(chan interface{})

	d.wg.Add(2)
	go d.notifyWhenReady(readyChan)
	d.daemonRunning = true

	// Run the daemon
	go func() {
		defer func() {
			defer d.wg.Done()
			go d.stopDaemon()
		}()

		chanDB, chanDBCleanUp, err := channeldbservice.Get(d.cfg.WorkingDir)
		if err != nil {
			d.log.Errorf("failed to create channeldbservice", err)
			return
		}
		c, err := chanDB.FetchAllChannels()
		if err != nil {
			d.log.Errorf("error when calling chanDB.FetchAllChannels(): %v", err)
		} else {
			if len(c) == 0 {
				d.startBeforeSync = false
			}
		}
		deleteZombies(chanDB)
		chainSevice, cleanupFn, err := chainservice.Get(d.cfg.WorkingDir, d.breezDB)
		if err != nil {
			chanDBCleanUp()
			d.log.Errorf("failed to create chainservice", err)
			return
		}
		deps := &Dependencies{
			workingDir:   d.cfg.WorkingDir,
			chainService: chainSevice,
			readyChan:    readyChan,
			chanDB:       chanDB}
		lndConfig, err := d.createConfig(deps.workingDir)
		if err != nil {
			d.log.Errorf("failed to create config %v", err)
		}
		d.log.Infof("Stating LND Daemon")
		err = lnd.Main(lndConfig, lnd.ListenerCfg{}, signal.ShutdownChannel(), deps)
		if err != nil {
			d.log.Errorf("Breez main function returned with error: %v", err)
		}
		d.log.Infof("LND Daemon Finished")

		chanDBCleanUp()
		cleanupFn()
	}()
	return nil
}

func (d *Daemon) createConfig(workingDir string) (*lnd.Config, error) {
	lndConfig := lnd.DefaultConfig()
	lndConfig.Bitcoin.Active = true
	if d.cfg.Network == "mainnet" {
		lndConfig.Bitcoin.MainNet = true
	} else if d.cfg.Network == "testnet" {
		lndConfig.Bitcoin.TestNet3 = true
	} else {
		lndConfig.Bitcoin.SimNet = true
	}
	lndConfig.LndDir = workingDir
	lndConfig.ConfigFile = path.Join(workingDir, "lnd.conf")

	cfg := lndConfig
	if err := flags.IniParse(lndConfig.ConfigFile, &cfg); err != nil {
		d.log.Errorf("Failed to parse config %v", err)
		return nil, err
	}
	if d.startBeforeSync {
		lndConfig.InitialHeadersSyncDelta = time.Hour * 2
	}

	writer, err := breezlog.GetLogWriter(workingDir)
	if err != nil {
		d.log.Errorf("GetLogWriter function returned with error: %v", err)
		return nil, err
	}
	cfg.LogWriter = writer
	cfg.MinBackoff = time.Second * 20
	cfg.Bitcoin.SkipChannelConfirmation = true
	conf, err := lnd.ValidateConfig(cfg, "")
	if err != nil {
		d.log.Errorf("ValidateConfig returned with error: %v", err)
		return nil, err
	}
	return conf, nil
}

func (d *Daemon) stopDaemon() {
	d.Lock()
	defer d.Unlock()
	if !d.daemonRunning {
		return
	}
	alive := signal.Alive()
	d.log.Infof("Daemon.stop() called, stopping breez daemon alive=%v", alive)
	if alive {
		signal.RequestShutdown()
	}
	close(d.quitChan)

	d.wg.Wait()
	d.daemonRunning = false
	d.ntfnServer.SendUpdate(DaemonDownEvent{})
	d.log.Infof("Daemon sent down event")
}

func (d *Daemon) notifyWhenReady(readyChan chan interface{}) {
	defer d.wg.Done()
	select {
	case <-readyChan:
		if err := d.startSubscriptions(); err != nil {
			d.log.Criticalf("Can't start daemon subscriptions, shutting down: %v", err)
			go d.stopDaemon()
		}
	case <-d.quitChan:
	}
}

func (d *Daemon) allChannelsActive(client lnrpc.LightningClient) (bool, error) {
	channels, err := client.ListChannels(context.Background(), &lnrpc.ListChannelsRequest{})
	if err != nil {
		d.log.Errorf("Error in allChannelsActive() > ListChannels(): %v", err)
		return false, err
	}
	for _, c := range channels.Channels {
		if !c.Active {
			return false, nil
		}
	}
	return true, nil
}
