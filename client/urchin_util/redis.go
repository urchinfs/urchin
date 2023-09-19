package urchin_util

import (
	"context"
	"github.com/go-redis/redis"
	redis3 "github.com/redis/go-redis/v9"
	"reflect"
	"strings"
	"time"
)

const (
	DefaultOpTimeout = -1
	keySeparator     = ":"
)

var RedisClusterIP = []string{"192.168.181.130:6379"}
var RedisClusterPwd = "dragonfly"

type RedisStorage struct {
	client redis.Cmdable
}

func NewRedisStorage(endpoints []string, password string, enableCluster bool) *RedisStorage {
	var client redis.Cmdable
	if enableCluster {
		client = redis.NewClusterClient(
			&redis.ClusterOptions{
				Addrs:       endpoints,
				Password:    password,
				DialTimeout: 3 * time.Second,
			},
		)
	} else if len(endpoints) != 0 {
		client = redis.NewClient(
			&redis.Options{
				Addr:     endpoints[0],
				Password: password,
			},
		)
	}
	if reflect.ValueOf(client).IsNil() {
		return nil
	}

	return &RedisStorage{
		client: client,
	}
}

func (s *RedisStorage) Get(key string) ([]byte, error) {
	exists, err := s.Exists(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrorNotExists
	}
	value, err := s.client.Get(key).Result()
	if err != nil {
		return nil, err
	}

	return []byte(value), nil
}

func (s *RedisStorage) Set(key string, value []byte) error {
	_, err := s.client.Set(key, value, DefaultOpTimeout).Result()
	if err != nil {
		return err
	}

	return nil
}

func (s *RedisStorage) SetWithTimeout(key string, value []byte, timeout time.Duration) error {
	_, err := s.client.Set(key, value, timeout).Result()
	if err != nil {
		return err
	}

	return nil
}

func (s *RedisStorage) SetNx(key string, value []byte, timeout time.Duration) (bool, error) {
	isExist, err := s.client.SetNX(key, value, timeout).Result()
	if err != nil {
		return false, err
	}

	return isExist, nil
}

func (s *RedisStorage) Del(key string) error {
	_, err := s.client.Del(key).Result()
	if err != nil {
		return err
	}

	return nil
}

func (s *RedisStorage) Exists(key string) (bool, error) {
	value, err := s.client.Exists(key).Result()
	if err != nil {
		return false, err
	}

	if value == int64(0) {
		return false, nil
	} else if value == int64(1) {
		return true, nil
	}

	return false, ErrorInternal
}

func (s *RedisStorage) InsertSet(key string, value string) error {
	_, err := s.client.SAdd(key, value).Result()
	if err != nil {
		return err
	}

	return nil
}

func (s *RedisStorage) DeleteSet(key string, members []string) error {
	if len(members) == 0 {
		return nil
	}

	sli := make([]interface{}, len(members))
	for i, v := range members {
		sli[i] = v
	}
	_, err := s.client.SRem(key, sli...).Result()
	if err != nil {
		return err
	}

	return nil
}

func (s *RedisStorage) ZRange(key string, start, stop int64) ([]string, error) {
	members, err := s.client.ZRange(key, start, stop).Result()
	if err != nil {
		return []string{}, err
	}

	return members, nil
}

func (s *RedisStorage) ZRangeByScore(key string, min, max string, offset, count int64) ([]string, error) {
	opt := redis.ZRangeBy{
		Min:    min,
		Max:    max,
		Offset: offset,
		Count:  count,
	}
	members, err := s.client.ZRangeByScore(key, opt).Result()
	if err != nil {
		return []string{}, err
	}

	return members, nil
}

func (s *RedisStorage) ZRevRangeByScore(key string, min, max string, offset, count int64) ([]string, error) {
	opt := redis.ZRangeBy{
		Min:    min,
		Max:    max,
		Offset: offset,
		Count:  count,
	}
	members, err := s.client.ZRevRangeByScore(key, opt).Result()
	if err != nil {
		return []string{}, err
	}

	return members, nil
}

func (s *RedisStorage) ZRem(key string, member string) error {
	_, err := s.client.ZRem(key, member).Result()
	if err != nil {
		return err
	}

	return nil
}

func (s *RedisStorage) ZAdd(key string, member string, score float64) error {
	_, err := s.client.ZAdd(key, redis.Z{
		Score:  score,
		Member: member,
	}).Result()
	if err != nil {
		return err
	}

	return nil
}

func (s *RedisStorage) ReadMap(key string) (map[string][]byte, error) {
	array, err := s.client.HKeys(key).Result()
	if err != nil {
		return nil, ErrorUnknown
	}

	result := make(map[string][]byte)
	for _, hKey := range array {
		value, err := s.client.HGet(key, hKey).Result()
		if err != nil {
			return nil, ErrorUnknown
		}
		result[hKey] = []byte(value)
	}

	return result, nil
}

func (s *RedisStorage) GetMapElement(key string, tag string) (string, error) {
	value, err := s.client.HGet(key, tag).Result()
	if err != nil {
		return "", err
	}

	return value, nil
}

func (s *RedisStorage) SetMapElement(key string, tag string, value []byte) error {
	_, err := s.client.HSet(key, tag, string(value)).Result()
	if err != nil {
		return err
	}
	return nil
}

func (s *RedisStorage) SetMapElements(key string, values map[string]interface{}) error {
	_, err := s.client.HMSet(key, values).Result()
	if err != nil {
		return err
	}
	return nil
}

func (s *RedisStorage) DeleteMap(key string) error {
	array, err := s.client.HKeys(key).Result()
	if err != nil {
		return ErrorUnknown
	}
	if len(array) == 0 {
		return nil
	}
	_, err = s.client.HDel(key, array...).Result()
	if err != nil {
		return err
	}

	return nil
}

func (s *RedisStorage) DeleteMapElement(key string, element string) error {
	_, err := s.client.HDel(key, element).Result()
	if err != nil {
		return err
	}
	return nil
}

func (s *RedisStorage) Scan(cursor uint64, match string, count int64) ([]string, uint64, error) {
	keys, nextCursor, err := s.client.Scan(cursor, match, count).Result()
	if err != nil {
		return []string{}, 0, err
	}
	return keys, nextCursor, nil
}

func (s *RedisStorage) GetTTL(key string) (time.Duration, error) {
	ttlDuration, err := s.client.TTL(key).Result()
	if err != nil {
		return -1, err
	}

	return ttlDuration, nil
}

func (s *RedisStorage) SetTTL(key string, ttlDuration time.Duration) error {
	_, err := s.client.Expire(key, ttlDuration).Result()
	if err != nil {
		return err
	}

	return nil
}

func (s *RedisStorage) Sort(key string, sort redis.Sort) ([]string, error) {
	members, err := s.client.Sort(key, &sort).Result()
	if err != nil {
		return []string{}, err
	}

	return members, nil
}

func (s *RedisStorage) MakeStorageKey(identifiers []string, prefix string) string {
	if len(prefix) == 0 {
		prefix = StoragePrefix
	}

	identifiers = append([]string{prefix}, identifiers...)
	key := strings.Join(identifiers, keySeparator)

	return key
}

type RedisStorage3 struct {
	client *redis3.Client
}

func NewRedisStorage3(endpoints []string, password string, enableCluster bool) *RedisStorage3 {
	var client *redis3.Client
	if enableCluster {
		//client = redis3.NewClusterClient(
		//	&redis3.ClusterOptions{
		//		Addrs:    endpoints,
		//		Password: password,
		//	},
		//)
	} else if len(endpoints) != 0 {
		client = redis3.NewClient(
			&redis3.Options{
				Addr:     endpoints[0],
				Password: password,
			},
		)
	}
	if reflect.ValueOf(client).IsNil() {
		return nil
	}

	return &RedisStorage3{
		client: client,
	}
}

func (s *RedisStorage3) Subscribe(ctx context.Context, channel string) (*redis3.PubSub, error) {
	pubSub := s.client.Subscribe(ctx, channel)

	return pubSub, nil
}

func (s *RedisStorage3) Receive(ctx context.Context, timeout time.Duration, pubSub *redis3.PubSub) (interface{}, error) {
	msg, err := pubSub.ReceiveTimeout(ctx, timeout)
	if err != nil {
		return nil, err
	}

	return msg, nil
}

func (s *RedisStorage3) Publish(ctx context.Context, channel string, message interface{}) (int64, error) {
	memberCnt, err := s.client.Publish(ctx, channel, message).Result()
	if err != nil {
		return -1, err
	}

	return memberCnt, nil
}

func (s *RedisStorage3) GetMapElement(ctx context.Context, key string, tag string) (string, error) {
	value, err := s.client.HGet(ctx, key, tag).Result()
	if err != nil {
		return "", err
	}

	return value, nil
}
