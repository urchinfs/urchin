package urchin_util

import (
	"d7y.io/dragonfly/v2/client/config"
	pkgstrings "d7y.io/dragonfly/v2/pkg/strings"
	"fmt"
	"math/rand"
	"time"
)

func GetReplicableDataSources(dynConfig config.Dynconfig, hostIp string) ([]string, error) {
	var seedPeerHosts []string
	schedulers, err := dynConfig.GetSchedulers()
	if err != nil {
		return nil, err
	}

	for _, scheduler := range schedulers {
		for _, seedPeer := range scheduler.SeedPeers {
			if hostIp != seedPeer.Ip && seedPeer.ObjectStoragePort > 0 {
				seedPeerHosts = append(seedPeerHosts, fmt.Sprintf("%s:%d", seedPeer.Ip, seedPeer.ObjectStoragePort))
			}
		}
	}
	seedPeerHosts = pkgstrings.Unique(seedPeerHosts)

	rand.Seed(time.Now().Unix())
	rand.Shuffle(len(seedPeerHosts), func(i, j int) {
		seedPeerHosts[i], seedPeerHosts[j] = seedPeerHosts[j], seedPeerHosts[i]
	})

	return seedPeerHosts, nil
}
