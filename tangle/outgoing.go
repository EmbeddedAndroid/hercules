package tangle

import (
	"bytes"
	"encoding/gob"
	"time"

	"../db"
	"../logs"
	"../server"
	"../utils"
	"github.com/dgraph-io/badger"
	"github.com/lukechampine/randmap"
)

const (
	tipRequestInterval = time.Duration(200) * time.Millisecond
	reRequestInterval  = time.Duration(10) * time.Second
	maxIncoming        = 100
)

type PendingRequest struct {
	Hash             []byte
	Timestamp        int
	LastTried        time.Time
	LastNeighborAddr string
}

var lastTip = time.Now()
var pendingRequests map[string]*PendingRequest

func Broadcast(data []byte, exclude string) int {
	sent := 0

	server.NeighborsLock.RLock()
	for _, neighbor := range server.Neighbors {
		server.NeighborsLock.RUnlock()

		if neighbor.Addr == exclude {
			server.NeighborsLock.RLock()
			continue
		}

		request := getSomeRequestByAddress(neighbor.Addr, false)
		sendReply(getMessage(data, request, request == nil, neighbor.Addr, nil))
		sent++

		server.NeighborsLock.RLock()
	}
	server.NeighborsLock.RUnlock()

	return sent
}

func pendingOnLoad() {
	pendingRequests = make(map[string]*PendingRequest)
	loadPendingRequests()
}

func loadPendingRequests() {
	// TODO: if pending is pending for too long, remove it from the loop?
	logs.Log.Info("Loading pending requests")

	db.Locker.Lock()
	defer db.Locker.Unlock()
	requestLocker.Lock()
	defer requestLocker.Unlock()

	total := 0
	added := 0

	_ = db.DB.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		it := txn.NewIterator(opts)
		defer it.Close()
		prefix := []byte{db.KEY_PENDING_HASH}
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			v, _ := item.Value()
			var hash []byte
			buf := bytes.NewBuffer(v)
			dec := gob.NewDecoder(buf)
			err := dec.Decode(&hash)
			if err == nil {
				timestamp, err := db.GetInt64(db.AsKey(item.Key(), db.KEY_PENDING_TIMESTAMP), txn)
				if err == nil {
					for _, neighbor := range server.Neighbors {
						queue, ok := requestQueues[neighbor.Addr]
						if !ok {
							q := make(RequestQueue, maxQueueSize)
							queue = &q
							requestQueues[neighbor.Addr] = queue
						}
						*queue <- &Request{hash, false}
					}
					addPendingRequest(hash, timestamp, "", false)
					added++
				} else {
					logs.Log.Warning("Could not load pending Tx Timestamp")
				}
			} else {
				logs.Log.Warning("Could not load pending Tx Hash")
			}
			total++
		}
		return nil
	})

	logs.Log.Info("Pending requests loaded", added, total)
}

func getSomeRequestByAddress(address string, any bool) []byte {
	requestLocker.RLock()
	requestQueue, requestOk := requestQueues[address]
	requestLocker.RUnlock()
	var request []byte
	if requestOk && len(*requestQueue) > 0 {
		request = (<-*requestQueue).Requested
	}
	if request == nil {
		pendingRequest := getOldPending(address)
		if pendingRequest == nil && any {
			pendingRequest = getAnyRandomOldPending(address)
		}
		if pendingRequest != nil {
			request = pendingRequest.Hash
		}
	}
	return request
}

func getSomeRequestByIPAddressWithPort(IPAddressWithPort string, any bool) []byte {
	neighbor := server.GetNeighborByIPAddressWithPort(IPAddressWithPort)

	// On low-end devices, the neighbor might already have gone until the message
	// is dequeued and processed. So, we need to check here if the neighbor is still there.
	if neighbor == nil {
		logs.Log.Debug("Neighbor gone:", IPAddressWithPort)
		return nil
	}

	return getSomeRequestByAddress(neighbor.Addr, any)
}

func outgoingRunner() {
	if len(srv.Incoming) > maxIncoming {
		return
	}

	shouldRequestTip := false
	if lowEndDevice {
		shouldRequestTip = time.Now().Sub(lastTip) > tipRequestInterval*5
	} else {
		shouldRequestTip = time.Now().Sub(lastTip) > tipRequestInterval
	}

	server.NeighborsLock.RLock()
	for _, neighbor := range server.Neighbors {
		server.NeighborsLock.RUnlock()

		var request = getSomeRequestByAddress(neighbor.Addr, false)
		ipAddressWithPort := server.GetFormattedAddress(neighbor.IP, neighbor.Port)
		if request != nil {
			sendReply(getMessage(nil, request, false, ipAddressWithPort, nil))
		} else if shouldRequestTip {
			lastTip = time.Now()
			sendReply(getMessage(nil, nil, true, ipAddressWithPort, nil))
		}

		server.NeighborsLock.RLock()
	}
	server.NeighborsLock.RUnlock()
}

func requestIfMissing(hash []byte, IPAddressWithPort string) (has bool, err error) {
	has = true
	if bytes.Equal(hash, tipFastTX.Hash) {
		return has, nil
	}
	key := db.GetByteKey(hash, db.KEY_HASH)
	if !db.Has(key, nil) && !db.Has(db.AsKey(key, db.KEY_PENDING_TIMESTAMP), nil) {
		pending := addPendingRequest(hash, 0, IPAddressWithPort, true)
		if pending != nil {
			neighbor := server.GetNeighborByIPAddressWithPort(IPAddressWithPort)
			if neighbor != nil {
				requestLocker.Lock()
				queue, ok := requestQueues[neighbor.Addr]
				if !ok {
					q := make(RequestQueue, maxQueueSize)
					queue = &q
					requestQueues[neighbor.Addr] = queue
				}
				requestLocker.Unlock()
				*queue <- &Request{Requested: hash, Tip: false}
			}
		}

		has = false
	}
	return has, nil
}

func sendReply(msg *Message) {
	if msg == nil {
		return
	}
	data := append((*msg.Bytes)[:1604], (*msg.Requested)[:46]...)
	srv.Outgoing <- &server.Message{IPAddressWithPort: msg.IPAddressWithPort, Msg: data}
	outgoing++
}

func getMessage(resp []byte, req []byte, tip bool, IPAddressWithPort string, txn *badger.Txn) *Message {
	var hash []byte
	if resp == nil {
		hash, resp = getRandomTip()
	}
	// Try getting latest milestone
	if resp == nil {
		milestone := LatestMilestone
		if milestone.TX != nil && milestone.TX != tipFastTX {
			resp = milestone.TX.Bytes
			if req == nil {
				hash = milestone.TX.Hash
			}
		}
	}
	/*/ Otherwise, latest (youngest) TX
	if resp == nil {
		key, _, _ := db.GetLatestKey(db.KEY_TIMESTAMP, false, txn)
		if key != nil {
			key = db.AsKey(key, db.KEY_BYTES)
			resp, _ = db.GetBytes(key, txn)
			if req == nil {
				key = db.AsKey(key, db.KEY_HASH)
				hash, _ = db.GetBytes(key, txn)
			}
		}
	}
	/**/
	// Random
	if resp == nil {
		resp = make([]byte, 1604)
		if req == nil {
			hash = make([]byte, 46)
		}
	}

	// If no request provided
	if req == nil {
		// Select tip, if so requested, or one of the random pending requests.
		if tip {
			req = hash
		} else {
			addr := IPAddressWithPort
			neighbor := server.GetNeighborByIPAddressWithPort(IPAddressWithPort)
			if neighbor != nil {
				addr = neighbor.Addr
			}
			pendingRequest := getOldPending(addr)
			if pendingRequest != nil {
				req = pendingRequest.Hash
			}
			if req == nil {
				pendingRequest = getAnyRandomOldPending(addr)
				if pendingRequest != nil {
					req = pendingRequest.Hash
				}
			}
		}
	}
	if req == nil {
		req = hash
	}
	if req == nil {
		req = make([]byte, 46)
	}
	return &Message{Bytes: &resp, Requested: &req, IPAddressWithPort: IPAddressWithPort}
}

func addPendingRequest(hash []byte, timestamp int64, IPAddressWithPort string, save bool) *PendingRequest {
	pendingRequestLocker.Lock()
	defer pendingRequestLocker.Unlock()

	key := string(hash)
	pendingRequest, ok := pendingRequests[key]

	if ok {
		return pendingRequest
	}

	if timestamp == 0 {
		timestamp = time.Now().Add(-reRequestInterval).Unix()
	}

	if save {
		key := db.GetByteKey(hash, db.KEY_PENDING_HASH)
		db.Put(key, hash, nil, nil)
		db.Put(db.AsKey(key, db.KEY_PENDING_TIMESTAMP), timestamp, nil, nil)
	}

	addr := IPAddressWithPort
	neighbor := server.GetNeighborByIPAddressWithPort(IPAddressWithPort)
	if neighbor != nil {
		addr = neighbor.Addr
	}

	pendingRequest = &PendingRequest{Hash: hash, Timestamp: int(timestamp), LastTried: time.Now(), LastNeighborAddr: addr}
	pendingRequests[key] = pendingRequest
	return pendingRequest
}

func removePendingRequest(hash []byte) bool {
	pendingRequestLocker.Lock()
	defer pendingRequestLocker.Unlock()

	key := string(hash)
	_, ok := pendingRequests[key]

	if ok {
		delete(pendingRequests, key)
		key := db.GetByteKey(hash, db.KEY_PENDING_HASH)
		db.Remove(key, nil)
		db.Remove(db.AsKey(key, db.KEY_PENDING_TIMESTAMP), nil)
	}
	return ok
}

func getOldPending(excludeAddress string) *PendingRequest {
	pendingRequestLocker.RLock()
	defer pendingRequestLocker.RUnlock()

	max := 1000
	if lowEndDevice {
		max = 200
	}

	length := len(pendingRequests)
	if length < max {
		max = length
	}

	for i := 0; i < max; i++ {
		k := randmap.FastKey(pendingRequests)
		v := pendingRequests[k.(string)]
		now := time.Now()
		if now.Sub(v.LastTried) > reRequestInterval && v.LastNeighborAddr != excludeAddress {
			v.LastTried = now
			v.LastNeighborAddr = excludeAddress
			return v
		}
	}

	return nil
}

func getAnyRandomOldPending(excludeAddress string) *PendingRequest {
	pendingRequestLocker.RLock()
	defer pendingRequestLocker.RUnlock()

	max := 10000
	if lowEndDevice {
		max = 300
	}

	length := len(pendingRequests)
	if length < max {
		max = length
	}

	if max > 0 {
		start := utils.Random(0, max)

		for i := start; i < max; i++ {
			k := randmap.FastKey(pendingRequests)
			v := pendingRequests[k.(string)]
			if v.LastNeighborAddr != excludeAddress {
				v.LastTried = time.Now()
				v.LastNeighborAddr = excludeAddress
				return v
			}
		}
	}

	return nil
}
