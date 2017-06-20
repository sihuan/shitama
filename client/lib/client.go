package client

import (
	"encoding/binary"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

type ShardInfoSlice []ShardInfo

func (s ShardInfoSlice) Len() int {
	return len(s)
}

func (s ShardInfoSlice) Less(i, j int) bool {
	return s[i].RTT < s[j].RTT
}

func (s ShardInfoSlice) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

type Client struct {
	logger    *logrus.Logger
	Config    map[string]interface{}
	Tunnel    *Tunnel
	API       *API
	connected bool
	shards    []ShardInfo
	link      *UDPLink
}

func NewClient() *Client {

	c := new(Client)

	c.logger = logrus.New()

	c.Config = make(map[string]interface{})
	c.Config["holderKcpAddr"] = "shitama.tldr.run:31337"
	c.Config["holderKcpAddrAlt"] = "115.159.87.170:31337"

	c.Tunnel = NewTunnel(c)
	c.API = NewAPI(c)

	c.shards = make([]ShardInfo, 0)
	c.link = nil

	c.Tunnel.OnConnected.SubscribeAsync("", func() {

		c.logger.WithFields(logrus.Fields{
			"scope": "client/handleConnected",
		}).Println("connected")

		c.connected = true

	}, false)

	c.Tunnel.OnDisconnected.SubscribeAsync("", func() {

		c.logger.WithFields(logrus.Fields{
			"scope": "client/handleDisconnected",
		}).Println("disconnected")

		c.connected = false

	}, false)

	return c

}

func (c *Client) Start() {

	c.Tunnel.Start()
	c.API.Start()

}

func (c *Client) GetStatus() map[string]interface{} {

	status := make(map[string]interface{})
	status["connected"] = c.connected

	return status

}

func (c *Client) UpdateShards() []ShardInfo {

	if !c.connected {
		return make([]ShardInfo, 0)
	}

	shards := c.Tunnel.zGetShards()

	if shards == nil {
		shards = make([]ShardInfo, 0)
	}

	c.updateRTTs(shards)

	sort.Sort(ShardInfoSlice(shards))

	c.shards = shards

	return shards

}

func (c *Client) RequestRelay(shardAddr string, transport string) (hostAddr string, guestAddr string) {

	if !c.connected {
		return "ERROR_UNCONNECTED", "ERROR_UNCONNECTED"
	}

	shard := c.findShardByAddr(shardAddr)

	if shard == nil {
		return "ERROR_SHARD_NOT_FOUND", "ERROR_SHARD_NOT_FOUND"
	}

	hostAddr, guestAddr = c.Tunnel.zShardRelay(shard.Addr, transport)

	if !strings.Contains(hostAddr, "ERROR") && !strings.Contains(guestAddr, "ERROR") {
		c.newLink(shard, hostAddr, transport)
	}

	return hostAddr, guestAddr

}

func (c *Client) GetConnectionStatus() map[string]interface{} {

	status := make(map[string]interface{})

	if c.link == nil {

		status["linkEstablished"] = false
		status["linkAddr"] = ""
		status["linkDelay"] = 0
		status["linkDelayDelta"] = 0
		status["peers"] = make([]interface{}, 0)

	} else {

		status["linkEstablished"] = true
		status["linkAddr"] = c.link.pc.LocalAddr().String()
		status["linkDelay"] = c.link.delay
		status["linkDelayDelta"] = c.link.delayDelta
		status["peers"] = make([]map[string]interface{}, 0)

		for _, dummy := range c.link.dummies {

			peer := make(map[string]interface{})
			peer["remoteAddr"] = dummy.peerAddr.String()
			peer["localAddr"] = dummy.pc.LocalAddr().String()
			peer["delay"] = dummy.delay
			// TODO
			//peer["profile"] = "key"
			peer["active"] = dummy.active.UnixNano()

			status["peers"] = append(status["peers"].([]map[string]interface{}), peer)

		}

	}

	return status

}

func (c *Client) findShardByAddr(addr string) *ShardInfo {

	for _, shard := range c.shards {
		if shard.Addr == addr {
			return &shard
		}
	}

	return nil

}

func (c *Client) updateRTTs(shards []ShardInfo) {

	type Pair struct {
		key   string
		value uint64
	}

	pc, err := net.ListenPacket("udp4", "0.0.0.0:0")

	if err != nil {
		c.logger.WithFields(logrus.Fields{
			"scope": "client/updateRTTs",
		}).Fatal(err)
	}

	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(time.Now().UnixNano()))

	for _, shard := range shards {
		addr, err := net.ResolveUDPAddr("udp4", shard.Addr)
		if err != nil {
			c.logger.WithFields(logrus.Fields{
				"scope": "client/updateRTTs",
			}).Warn(err)
			continue
		}
		for i := 0; i < 16; i++ {
			pc.WriteTo(buf, addr)
		}
	}

	shardRTTs := make(map[string][]uint64)

	pairs := make(chan Pair)

	go (func() {

		for {

			_, addr, err := pc.ReadFrom(buf)

			if err != nil {
				c.logger.WithFields(logrus.Fields{
					"scope": "client/updateRTTs",
				}).Warn(err)
				break
			}

			key := addr.String()
			now := uint64(time.Now().UnixNano())
			then := binary.BigEndian.Uint64(buf)

			pairs <- Pair{key: key, value: now - then}

		}

	})()

WaitLoop:
	for {
		select {
		case v := <-pairs:
			if _, ok := shardRTTs[v.key]; !ok {
				shardRTTs[v.key] = make([]uint64, 0)
			}
			shardRTTs[v.key] = append(shardRTTs[v.key], v.value)
			break
		case <-time.After(1 * time.Second):
			break WaitLoop
		}
	}

	pc.Close()

	for idx := range shards {
		shard := &shards[idx]
		if rtts, ok := shardRTTs[shard.Addr]; ok {
			var sum uint64
			for _, v := range rtts {
				sum += v
			}
			shard.RTT = float32(sum) / 1e6 / float32(len(rtts))
		}
	}

}

func (c *Client) newLink(shard *ShardInfo, hostAddr string, transport string) {

	if c.link != nil {
		c.link.Stop()
	}

	addr, err := net.ResolveUDPAddr("udp4", hostAddr)

	if err != nil {
		c.logger.WithFields(logrus.Fields{
			"scope": "client/newLink",
		}).Fatal(err)
	}

	switch transport {
	case "udp":

		shardAddr, err := net.ResolveUDPAddr("udp4", shard.Addr)
		if err != nil {
			c.logger.WithFields(logrus.Fields{
				"scope": "client/newLink",
			}).Warn(err)
		}

		c.link = NewUDPLink(c, shardAddr, addr)
		c.link.Start()

		break

	}

}
