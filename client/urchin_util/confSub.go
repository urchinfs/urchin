package urchin_util

import (
	"context"
	"log"
	"strconv"
	"sync"
	"time"
)

const (
	ConfMonitorTimeOUt = 30
	ConfPubPrefix      = "urchin:conf:pub"
)

type redisInfo struct {
	endpoints     []string
	password      string
	enableCluster bool
}

type ConfSub interface {
	Subscribe(ctx context.Context, channel string, f func(channel, payload string) error) error
	UnSubscribe(ctx context.Context, channel string)
}

type confSub struct {
	info        redisInfo
	redisClient *RedisStorage3
	timestamp   time.Duration
	stopped     bool
}

func (c confSub) Init(endpoints []string, password string, enableCluster bool) {
	c.info.endpoints = endpoints
	c.info.password = password
	c.info.enableCluster = enableCluster
}

func (c confSub) Subscribe(ctx context.Context, channel string, f func(channel, payload string) error) error {
	if len(c.info.endpoints) <= 0 {
		return ErrorInvalidParameter
	}

	if c.redisClient == nil {
		c.redisClient = NewRedisStorage3(c.info.endpoints, c.info.password, c.info.enableCluster)
		if c.redisClient == nil {
			return ErrorInvalidParameter
		}
	}

	pubSub, err := c.redisClient.Subscribe(ctx, channel)
	if err != nil {
		return err
	}

	val, err := c.redisClient.GetMapElement(ctx, ConfPubPrefix, "timestamp")
	if err == nil {
		timestamp, _ := strconv.Atoi(val)
		c.timestamp = time.Duration(timestamp)
	}

	go func() {
		tickTimer := time.NewTicker(ConfMonitorTimeOUt * time.Second)
		ch := pubSub.Channel()
		for {
			select {
			case msg := <-ch:
				err := f(msg.Channel, msg.Payload)
				if err != nil {
					log.Printf("process fail Channel:%s, Payload:%s, Err:%v", msg.Channel, msg.Payload, err)
				}
			case <-tickTimer.C:
				val, _ = c.redisClient.GetMapElement(ctx, ConfPubPrefix, "timestamp")
				timestamp, _ := strconv.Atoi(val)
				if time.Duration(timestamp) > c.timestamp {
					channelRev, err := c.redisClient.GetMapElement(ctx, ConfPubPrefix, "channel")
					if err != nil {
						continue
					}

					payloadRev, err := c.redisClient.GetMapElement(ctx, ConfPubPrefix, "payload")
					if err != nil {
						continue
					}

					err = f(channelRev, payloadRev)
					if err != nil {
						log.Printf("process fail Channel:%s, Payload:%s, Err:%v", channelRev, payloadRev, err)
					}

					c.timestamp = time.Duration(timestamp)
				}
			}

			if c.stopped {
				break
			}
		}
	}()

	return nil
}

func (c confSub) UnSubscribe(ctx context.Context, channel string) {
	c.stopped = true
	c.redisClient = nil
}

var conf *confSub
var once sync.Once

func GetConfSub() ConfSub {
	once.Do(func() {
		conf = &confSub{}
	})

	return *conf
}
