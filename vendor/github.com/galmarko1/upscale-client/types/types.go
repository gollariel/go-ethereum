package types

import (
	"encoding/hex"
	"github.com/galmarko1/upscale-client/protobuf"
	"sync"
	"time"
)

const GracePeriod = 15 * time.Second

type ID [32]byte

func (id ID) String() string {
	return hex.EncodeToString(id[:])
}

type Peer struct {
	Info       protobuf.PeerInfo
	Connected  time.Time
	LastUpdate time.Time
	AddedLast  int32
}
type Peers map[ID]*Peer

func (p *Peer) IsGracePeriod() bool {
	return p.Connected == p.LastUpdate && time.Since(p.Connected) < GracePeriod && p.AddedLast == 0
}

type tx struct {
	loader   ID
	loadTime time.Time
}

func (tx tx) GetLoader() ID {
	return tx.loader
}

type txs map[string]tx

type LoadedTxs struct {
	lock *sync.Mutex
	txs
}

func NewLoadedTxs() LoadedTxs {
	return LoadedTxs{
		lock: &sync.Mutex{},
		txs:  make(txs),
	}
}

func (lt LoadedTxs) Load(hash string, loader ID) {
	lt.lock.Lock()
	defer lt.lock.Unlock()
	lt.txs[hash] = tx{loader: loader, loadTime: time.Now()}
}

func (lt LoadedTxs) Delete(hash string) *tx {
	lt.lock.Lock()
	defer lt.lock.Unlock()
	tx, ok := lt.txs[hash]
	if !ok {
		return nil
	}
	delete(lt.txs, hash)
	return &tx
}

func (lt LoadedTxs) DeleteByLoader(loader ID) int {
	lt.lock.Lock()
	defer lt.lock.Unlock()
	deleted := 0
	for hash, tx := range lt.txs {
		if tx.loader == loader {
			delete(lt.txs, hash)
			deleted++
		}
	}
	return deleted
}

func (lt LoadedTxs) DeleteByAge(age int) int {
	lt.lock.Lock()
	defer lt.lock.Unlock()
	deleted := 0
	for hash, tx := range lt.txs {
		txAge := int(time.Since(tx.loadTime).Seconds())
		if txAge > age {
			delete(lt.txs, hash)
			deleted++
		}
	}
	return deleted
}
