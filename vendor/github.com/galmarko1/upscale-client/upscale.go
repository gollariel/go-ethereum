package upscale_client

import (
	"context"
	"encoding/hex"
	"github.com/galmarko1/upscale-client/protobuf"
	"github.com/galmarko1/upscale-client/types"
	"github.com/orandin/lumberjackrus"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"io/ioutil"
	"net"
	"os"
	"sync"
	"time"
)

type upscale struct {
	lock             sync.Mutex
	activePeers      types.Peers
	loadedTxs        types.LoadedTxs
	client           protobuf.UpscaleClient
	contributionChan chan *protobuf.ContributionRequest
	metadata         metadata.MD
}

var (
	self     *upscale
	disabled = true
)

// initLog is called only when Init is called
func initLog() string {
	log.SetFormatter(&log.TextFormatter{})
	log.SetLevel(log.TraceLevel)

	// send logs to nowhere by default, use hooks for separate stdout/stderr
	log.SetOutput(ioutil.Discard)

	logFile := os.Getenv("upscale_log")
	if logFile == "" {
		logFile = "/tmp/upscale.log"
	}

	level := os.Getenv("upscale_log_level")
	if level == "" {
		level = "info"
	}
	logLevel, err := log.ParseLevel(level)
	if err != nil {
		logLevel = log.InfoLevel
	}

	hook, err := lumberjackrus.NewHook(
		&lumberjackrus.LogFile{
			Filename:   logFile,
			MaxSize:    10,
			MaxBackups: 3,
			MaxAge:     1,
			Compress:   false,
			LocalTime:  false,
		},
		logLevel,
		&log.TextFormatter{},
		&lumberjackrus.LogFileOpts{},
	)

	if err != nil {
		panic(err)
	}

	log.AddHook(hook)
	return logLevel.String()
}

// Init initialise upscale - log, server connection, etc
func Init(id string) {
	if !disabled {
		log.Warn("multiple Init calls")
		return
	}
	logLevel := initLog()
	if len(id) != 64 {
		log.Panicf("invalid id %v. id should be 64 chars in size. Please check intergartion")
	}

	us := upscale{
		activePeers:      make(types.Peers),
		loadedTxs:        types.NewLoadedTxs(),
		contributionChan: make(chan *protobuf.ContributionRequest, 100),
	}
	self = &us

	server := os.Getenv("upscale_addr")
	user := os.Getenv("upscale_user")
	password := os.Getenv("upscale_password")

	log.Infof("initialising upscale - server at %v, user %v, password ******, logLevel %v", server, user, logLevel)

	conn, err := grpc.Dial(server, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Errorf("failed to initilaise connection with upscale server at %v - %v", server, err)
		log.Info("upscale is disabled")
		return
	}
	us.client = protobuf.NewUpscaleClient(conn)
	us.metadata = metadata.Pairs("id", id, "user", user, "password", password)

	go us.run()
	go us.sendContributions()
	disabled = false
}

func (us *upscale) run() {
	ticker := time.NewTicker(60 * time.Second)
	for {
		<-ticker.C
		us.lock.Lock()
		newTxs := int32(0)
		entries := make([]*protobuf.Entry, 0)
		for _, peer := range us.activePeers {
			if peer.IsGracePeriod() {
				continue
			}
			entry := &protobuf.Entry{
				PeerInfo: &peer.Info,
				TxAdded:  peer.AddedLast,
				TimeSpan: int32(time.Since(peer.LastUpdate).Seconds()),
			}
			entries = append(entries, entry)
			newTxs += peer.AddedLast

			peer.AddedLast = 0
			peer.LastUpdate = time.Now()
		}

		us.lock.Unlock()

		log.Tracef("loaded %v transactions, peers connected %v, sending update for %v peers", newTxs,
			len(us.activePeers), len(entries))

		contributionRequest := &protobuf.ContributionRequest{
			TypeOfUpdate: protobuf.TypeOfUpdate_normal,
			Entries:      entries,
		}
		us.sendContribution(contributionRequest)
		deleted := us.loadedTxs.DeleteByAge(5 * 60)
		log.Tracef("deleted %v old loaded txs", deleted)
	}
}

func (us *upscale) sendContribution(req *protobuf.ContributionRequest) {
	select {
	case us.contributionChan <- req:
	default:
		log.Error("distribution request channel is full. dropping")
	}
}

// sendContributions is a go routine that sends contributions messages to upscale server
func (us *upscale) sendContributions() {
	ctx := metadata.NewOutgoingContext(context.Background(), us.metadata)
	var client protobuf.Upscale_ContributionClient
	var connected = false
	var err error
	for {
		req := <-us.contributionChan
		if !connected {
			client, err = us.client.Contribution(ctx)
			if err != nil {
				log.Errorf("dropping contribution message - %v", err)
				continue
			}
			connected = true
		}
		err := client.Send(req)
		if err != nil {
			log.Infof("connection with server lost - %v. Message lost. Reconnecting.", err)
			connected = false
		}
	}
}

func PeerAdded(_ int, id types.ID, flags string, remoteAddr net.Addr, name string, enode string) {
	if disabled {
		return
	}
	self.lock.Lock()
	defer self.lock.Unlock()
	_, ok := self.activePeers[id]
	if ok {
		log.Errorf("peer %v being added, is already active", id)
		return
	}
	addedAt := time.Now()
	self.activePeers[id] = &types.Peer{
		Info: protobuf.PeerInfo{
			Flags:         flags,
			RemoteAddress: remoteAddr.String(),
			Name:          name,
			Enode:         enode,
			PeerID: &protobuf.PeerID{
				ID: id[:],
			},
		},
		Connected:  addedAt,
		LastUpdate: addedAt,
	}
	log.Tracef("peer %v added", id)
}

func PeerRemoved(_ int, id types.ID) {
	if disabled {
		return
	}
	self.lock.Lock()
	peer, ok := self.activePeers[id]
	if !ok {
		log.Errorf("peer %v being removed, is not active", id)
		return
	}
	delete(self.activePeers, id)
	self.lock.Unlock()

	switch {
	case peer.IsGracePeriod():
		// no need to concern with short term peer with no contribution
	default:
		entries := make([]*protobuf.Entry, 1)
		entries[0] = &protobuf.Entry{
			PeerInfo: &peer.Info,
			TxAdded:  peer.AddedLast,
			TimeSpan: int32(time.Since(peer.LastUpdate).Seconds()),
		}
		contributionRequest := &protobuf.ContributionRequest{
			TypeOfUpdate: protobuf.TypeOfUpdate_final,
			Entries:      entries,
		}
		self.sendContribution(contributionRequest)
		loader := types.ID{}
		copy(loader[:], peer.Info.PeerID.ID)
		deleted := self.loadedTxs.DeleteByLoader(loader)
		log.Tracef("deleted %v loaded txs for %v", deleted, loader.String())
	}
	log.Tracef("peer %v removed", id)
}

// UpScale is called in Ethereum client context
func UpScale(id types.ID, _ net.Addr, peers int, maxPeers int, inboundCount int, maxInbound int, isInbound, isTrusted bool) (replace bool, replaceID types.ID) {
	if disabled {
		return
	}
	log.Tracef("upscale call for %v", id)
	// if trusted we accept
	if isTrusted {
		return false, replaceID
	}

	// if inbound and we are not max out we accept
	if isInbound && inboundCount < maxInbound {
		return false, replaceID
	}

	if !isInbound && peers < maxPeers {
		return false, replaceID
	}

	us := self
	ctx := metadata.NewOutgoingContext(context.Background(), self.metadata)
	req := &protobuf.OpportunityRequest{PeerInfo: &protobuf.PeerInfo{
		PeerID:        &protobuf.PeerID{ID: id[:]},
		RemoteAddress: "",
	}}
	resp, err := us.client.Opportunity(ctx, req)
	if err != nil {
		log.Errorf("got error from Opportunity - %v", err)
		return false, replaceID
	}
	if !resp.Replace {
		return false, replaceID
	}

	copy(replaceID[:], resp.ReplaceOptions[0].ID[:])
	log.Debug("done upscale call for %v: replace %v with %v", id, resp.Replace)
	return resp.Replace, replaceID
}

func TransactionAdded(hash string, source string) {
	if disabled {
		return
	}
	self.lock.Lock()
	defer self.lock.Unlock()
	sourceID := types.ID{}
	id, err := hex.DecodeString(source)
	if err != nil {
		log.Errorf("failed to convert %v to []byte - %v", source, err)
	}
	copy(sourceID[:], id)
	self.loadedTxs.Load(hash, sourceID)
	log.Tracef("tx %v loaded by %v", hash, source)
}

func BlockTransactions(hashes []string) {
	if disabled {
		return
	}
	self.lock.Lock()
	defer self.lock.Unlock()
	found := 0
	for _, hash := range hashes {
		tx := self.loadedTxs.Delete(hash)
		if tx != nil {
			loader := tx.GetLoader()
			peer, ok := self.activePeers[loader]
			if !ok {
				log.Warnf("failed to fetch contributor peer %v for tx %v", loader.String(), hash)
				continue
			}
			peer.AddedLast++
			found++
		}
	}
	log.Debugf("found %v txs out of %v", found, len(hashes))
}
