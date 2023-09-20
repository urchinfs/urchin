package urchin_peers

import (
	"crypto/md5"
	"d7y.io/api/pkg/apis/scheduler/v1"
	"d7y.io/dragonfly/v2/client/config"
	"d7y.io/dragonfly/v2/client/daemon/urchin_dataset"
	"d7y.io/dragonfly/v2/client/urchin_util"
	logger "d7y.io/dragonfly/v2/internal/dflog"
	"encoding/hex"
	"fmt"
	"github.com/docker/go-units"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type UrchinPeer struct {
	PeerHost      *scheduler.PeerHost
	StorageConfig *config.StorageOption
	PeerPort      int
}

func NewPeer(peerHost *scheduler.PeerHost, storageConfig *config.StorageOption) *UrchinPeer {
	urchinPeer := &UrchinPeer{
		PeerHost:      peerHost,
		StorageConfig: storageConfig,
	}

	go func() {
		time.Sleep(time.Second * 2)
		if urchinPeer.PeerPort != 0 {
			err := urchinPeer.initReportPeerInfo()
			if err != nil {
				logger.Warnf("urchinPeer init failed:%v", err)
				return
			}
		}

		urchinPeer.periodReportPeerInfo()
	}()

	return urchinPeer
}

func (up *UrchinPeer) CheckPeerIdValid(peerID string) bool {
	if len(peerID) < 16 {
		return false
	}

	return true
}

func (up *UrchinPeer) GetPeer(ctx *gin.Context) {
	var params UrchinPeerParams
	if err := ctx.ShouldBindUri(&params); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var (
		peerID = params.ID
	)

	if !up.CheckPeerIdValid(peerID) {
		logger.Warnf("GetPeer peerID is invalid, peerId:%s", peerID)
		ctx.JSON(http.StatusBadRequest, gin.H{"errors": "parameter error"})
		return
	}
	logger.Infof("GetPeer peerID id:%s %s", peerID)

	redisClient := urchin_util.NewRedisStorage(urchin_util.RedisClusterIP, urchin_util.RedisClusterPwd, false)
	peerInfo, err := getPeerInfoById(peerID, redisClient)
	if err != nil {
		logger.Warnf("GetPeer fail, peerId:%s", peerID)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	if peerInfo.Id != "" {
		ctx.JSON(http.StatusOK, gin.H{
			"status_code": 0,
			"status_msg":  "succeed",
			"peer":        peerInfo,
		})

		return
	}

	logger.Warnf("GetPeer not found peer, peerId:%s", peerID)
	ctx.JSON(http.StatusNotFound, gin.H{"errors": err.Error()})
	return
}

func GetUrchinPeers(ctx *gin.Context) {
	var form UrchinPeerQueryParams
	if err := ctx.ShouldBind(&form); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var (
		pageIndex        = form.PageIndex
		pageSize         = form.PageSize
		searchKey        = form.SearchKey
		orderBy          = form.OrderBy
		sortBy           = form.SortBy
		createdAtLess    = form.CreatedAtLess
		createdAtGreater = form.CreatedAtGreater
	)

	if orderBy == "domain" {
		orderBy = "security_domain"
	} else if orderBy == "center_id" {
		orderBy = "location"
	}

	getCacheSortSet := func() string {
		formPara := searchKey + orderBy + fmt.Sprint(sortBy) + fmt.Sprint(createdAtLess) + fmt.Sprint(createdAtGreater)
		h := md5.New()
		h.Write([]byte(formPara))

		curTime := time.Now()
		return PeersInfoTmpSortSet + "_" + hex.EncodeToString(h.Sum(nil)) + "_" + fmt.Sprint(curTime.Unix()-int64(curTime.Second()%20))
	}

	var peerInfos []UrchinPeerInfo
	redisClient := urchin_util.NewRedisStorage(urchin_util.RedisClusterIP, urchin_util.RedisClusterPwd, false)
	peersInfoCreateQue := redisClient.MakeStorageKey([]string{PeersInfoCreateQue}, storagePrefixPeer)
	if searchKey == "" {
		if orderBy == "" {
			var rangeLower, rangeUpper int64
			if createdAtLess != 0 {
				rangeUpper = createdAtLess
			} else {
				rangeUpper = time.Now().Unix() + 1
			}
			if createdAtGreater != 0 {
				rangeLower = createdAtGreater
			} else {
				rangeLower = 0
			}

			var members []string
			var err error
			if sortBy == 1 {
				members, err = redisClient.ZRangeByScore(peersInfoCreateQue, strconv.FormatInt(rangeLower, 10), strconv.FormatInt(rangeUpper, 10), int64(pageIndex), int64(pageSize))
			} else {
				members, err = redisClient.ZRevRangeByScore(peersInfoCreateQue, strconv.FormatInt(rangeLower, 10), strconv.FormatInt(rangeUpper, 10), int64(pageIndex), int64(pageSize))
			}

			if err != nil {
				logger.Infof("GetUrchinPeers range by score err:%v", err)
				ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
				return
			}

			for _, member := range members {
				peerInfo, err := getPeerInfoById(member, redisClient)
				if err != nil {
					logger.Infof("GetUrchinPeers get peer by id err:%v", err)
					ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
					continue
				}

				peerInfos = append(peerInfos, peerInfo)
			}
		} else {
			var tmpSortSetKey string
			if createdAtLess != 0 || createdAtGreater != 0 {
				tmpSortSetKey = redisClient.MakeStorageKey([]string{getCacheSortSet()}, storagePrefixPeer)
				exists, err := redisClient.Exists(tmpSortSetKey)
				if err != nil || !exists {
					var members []string
					err := urchin_dataset.MatchZSetMemberByCreateTime(createdAtLess, createdAtGreater, peersInfoCreateQue, &members, redisClient)
					if err != nil {
						logger.Infof("GetUrchinPeers match peer by create time err:%v", err)
						ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
						return
					}

					tmpSortSetKey = redisClient.MakeStorageKey([]string{getCacheSortSet()}, storagePrefixPeer)
					err = urchin_dataset.WriteToTmpSet(members, tmpSortSetKey, redisClient)
					if err != nil {
						logger.Infof("GetUrchinPeers write to tmp set err:%v", err)
						ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
						return
					}
				}

			} else {
				tmpSortSetKey = peersInfoCreateQue
			}

			err := sortAndBuildResult(orderBy, sortBy, pageIndex, pageSize, tmpSortSetKey, redisClient, &peerInfos)
			if err != nil {
				logger.Infof("GetUrchinPeers sort and build result err:%v", err)
				ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
				return
			}
		}
	} else {
		var tmpSortSetKey string

		tmpSortSetKey = redisClient.MakeStorageKey([]string{getCacheSortSet()}, storagePrefixPeer)
		exists, err := redisClient.Exists(tmpSortSetKey)
		if err != nil || !exists {
			matchPrefix := make(map[string]bool)
			prefix := storagePrefixPeer + "*" + "match_peer_prefix:*" + searchKey + "*"
			err := urchin_dataset.MatchKeysByPrefix(prefix, matchPrefix, redisClient)
			if err != nil {
				logger.Infof("GetUrchinPeers match peer by name prefix err:%v, prefix:%s", err, prefix)
				ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
				return
			}

			var matchResult []string
			if createdAtLess != 0 || createdAtGreater != 0 {
				err = urchin_dataset.MatchZSetMemberByCreateTime(createdAtLess, createdAtGreater, peersInfoCreateQue, &matchResult, redisClient)
				if err != nil {
					logger.Infof("GetUrchinPeers match peer by create time err:%v", err)
					ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
					return
				}
			}

			matchCreateTime := make(map[string]bool)
			for _, member := range matchResult {
				matchCreateTime[member] = true
			}

			if createdAtLess != 0 || createdAtGreater != 0 {
				matchPrefix = urchin_dataset.InterMap(matchPrefix, matchCreateTime)
			}

			matchSlice := urchin_dataset.MapToSlice(matchPrefix)
			tmpSortSetKey = redisClient.MakeStorageKey([]string{getCacheSortSet()}, storagePrefixPeer)
			err = urchin_dataset.WriteToTmpSet(matchSlice, tmpSortSetKey, redisClient)
			if err != nil {
				logger.Infof("GetUrchinPeers write to tmp set err:%v", err)
				ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
				return
			}
		}

		err = sortAndBuildResult(orderBy, sortBy, pageIndex, pageSize, tmpSortSetKey, redisClient, &peerInfos)
		if err != nil {
			logger.Infof("GetUrchinPeers sort and build result err:%v", err)
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}
	}

	ctx.JSON(http.StatusOK, gin.H{
		"status_code": 0,
		"status_msg":  "succeed",
		"peers":       peerInfos,
	})
}

func (up *UrchinPeer) initReportPeerInfo() error {
	redisClient := urchin_util.NewRedisStorage(urchin_util.RedisClusterIP, urchin_util.RedisClusterPwd, false)
	peerId := up.PeerHost.Id
	peerInfoKey := redisClient.MakeStorageKey([]string{up.PeerHost.Id}, storagePrefixPeer)
	values := make(map[string]interface{})
	values["id"] = peerId
	values["security_domain"] = up.PeerHost.Idc
	values["location"] = up.PeerHost.Location
	values["hostname"] = up.PeerHost.Hostname
	values["ip"] = up.PeerHost.Ip
	values["peer_port"] = up.PeerPort
	values["store_strategy"] = string(up.StorageConfig.StoreStrategy)
	values["task_expire_time"] = int64(up.StorageConfig.TaskExpireTime.Duration / time.Second)
	values["disk_gc_threshold"] = int64(up.StorageConfig.DiskGCThreshold)

	curTime := time.Now().Unix()
	values["create_time"] = strconv.FormatInt(curTime, 10)
	values["update_time"] = strconv.FormatInt(curTime, 10)

	err := redisClient.SetMapElements(peerInfoKey, values)
	if err != nil {
		logger.Warnf("initReportPeerInfo set map elements err:%v, peerInfoKey:%s", err, peerInfoKey)
		return err
	}

	err = redisClient.ZAdd(redisClient.MakeStorageKey([]string{PeersInfoCreateQue}, storagePrefixPeer), peerId, float64(curTime))
	if err != nil {
		logger.Warnf("initReportPeerInfo add zset key err:%v, peerInfoKey:%s", err, peerInfoKey)
		return err
	}

	prefixStr := up.getMatchPrefix()
	datasetNameKey := redisClient.MakeStorageKey([]string{peerId, "match_peer_prefix", prefixStr}, storagePrefixPeer)
	err = redisClient.Set(datasetNameKey, []byte(prefixStr))
	if err != nil {
		logger.Infof("initReportPeerInfo set peer match prefix err:%v, peerId:%s", err, peerId)
		return err
	}

	logger.Warnf("initReportPeerInfo set map elements ok, peerInfoKey:%s", err, peerInfoKey)
	return nil
}

func (up *UrchinPeer) periodReportPeerInfo() {
	redisClient := urchin_util.NewRedisStorage(urchin_util.RedisClusterIP, urchin_util.RedisClusterPwd, false)
	peersInfoExpireLock := redisClient.MakeStorageKey([]string{PeersInfoExpireLock}, storagePrefixPeer)
	peersInfoExpireQue := redisClient.MakeStorageKey([]string{PeersInfoExpireQue}, storagePrefixPeer)

	time.Sleep(time.Second * 10)
	for {
		time.Sleep(time.Second * 7)
		peerInfoKey := redisClient.MakeStorageKey([]string{up.PeerHost.Id}, storagePrefixPeer)
		exists, err := redisClient.Exists(peerInfoKey)
		if err != nil {
			logger.Warnf("peer host get peerId key:%s exist err:%v, peerId:%s", err, peerInfoKey)
			continue
		}

		peerId := up.PeerHost.Id
		if !exists {
			err := up.initReportPeerInfo()
			if err != nil {
				logger.Warnf("peer host init peer info map err:%v, peerId:%s", err, peerInfoKey)
			}
			continue
		}

		ttl, err := redisClient.GetTTL(peerInfoKey)
		if err == nil && ttl < PeersInfoExpireTime*time.Second/10 {
			_ = redisClient.SetTTL(peerInfoKey, PeersInfoExpireTime*time.Second)
		}

		curTime := time.Now().Unix()
		err = redisClient.ZAdd(peersInfoExpireQue, peerId, float64(curTime))
		if err != nil {
			logger.Warnf("peer host ZAdd err:%v, peerId:%s", err, peerId)
		}

		isExist, err := redisClient.SetNx(peersInfoExpireLock, []byte(peersInfoExpireLock), PeersLockExpireTime*time.Second)
		if err != nil {
			logger.Warnf("peer host SetNx err:%v, peerId:%s", err, peerId)
			continue
		}
		if !isExist {
			continue
		}

		curTime = time.Now().Unix()
		expireTime := curTime - PeersScoreExpireTime
		var offset int64 = 0
		var count int64 = 100
		for {
			members, err := redisClient.ZRangeByScore(peersInfoExpireQue, strconv.Itoa(0), strconv.FormatInt(expireTime, 10), offset, count)
			if err != nil {
				logger.Warnf("peer host ZRangeByScore err:%v, peerId:%s", err, peerId)
				continue
			}

			for _, member := range members {
				logger.Infof("del peer info, id:%s, curTime:%d", member, curTime)

				err := redisClient.ZRem(peersInfoExpireQue, member)
				if err != nil {
					logger.Warnf("peer host ZRem err:%v, peerId:%s", err, member)
				}

				err = redisClient.ZRem(redisClient.MakeStorageKey([]string{PeersInfoCreateQue}, storagePrefixPeer), member)
				if err != nil {
					logger.Warnf("peer host ZRem err:%v, peerId:%s", err, member)
				}

				err = redisClient.Del(peerInfoKey)
				if err != nil {
					logger.Warnf("peer host Del peerInfoKey err:%v, peerId:%s", err, member)
				}

				peerMatchPrefixKey := redisClient.MakeStorageKey([]string{peerId, "match_peer_prefix", up.getMatchPrefix()}, storagePrefixPeer)
				err = redisClient.Del(peerMatchPrefixKey)
				if err != nil {
					logger.Infof("peer host loop del match prefix key %s err:%v", peerMatchPrefixKey, err)
				}
			}

			if len(members) <= 0 {
				break
			}

			offset += count
		}

		time.Sleep(time.Second * 2)
	}
}

func (up *UrchinPeer) getMatchPrefix() string {
	return strings.Join([]string{up.PeerHost.Idc, up.PeerHost.Location, up.PeerHost.Hostname, up.PeerHost.SecurityDomain}, "_")
}

func getPeerInfoById(peerId string, redisClient *urchin_util.RedisStorage) (UrchinPeerInfo, error) {
	var peerInfo UrchinPeerInfo
	peerInfoKey := redisClient.MakeStorageKey([]string{peerId}, storagePrefixPeer)
	elements, err := redisClient.ReadMap(peerInfoKey)
	if err != nil {
		logger.Infof("getPeerInfoById read map element err:%v, peerId:%s", err, peerId)
		return peerInfo, err
	}

	for k, v := range elements {
		if k == "id" {
			peerInfo.Id = string(v)
		} else if k == "security_domain" {
			peerInfo.SecurityDomain = string(v)
		} else if k == "location" {
			peerInfo.Location = string(v)
		} else if k == "hostname" {
			peerInfo.Hostname = string(v)
		} else if k == "ip" {
			peerInfo.Ip = string(v)
		} else if k == "peer_port" {
			var tmpV int
			tmpV, err = strconv.Atoi(string(v))
			peerInfo.PeerPort = tmpV
		} else if k == "store_strategy" {
			peerInfo.StoreStrategy = string(v)
		} else if k == "task_expire_time" {
			var tmpV int
			tmpV, err = strconv.Atoi(string(v))
			peerInfo.TaskExpireTime = int64(tmpV)
		} else if k == "disk_gc_threshold" {
			var tmpV float64
			tmpV, err = strconv.ParseFloat(string(v), 10)
			peerInfo.DiskGCThreshold = units.BytesSize(tmpV)
		}

		if err != nil {
			logger.Warnf("getPeerInfoById read element err:%v, peerInfoKey:%s", err, peerInfoKey)
			return peerInfo, err
		}
	}

	return peerInfo, nil
}

func sortAndBuildResult(orderBy string, sortBy int, pageIndex, pageSize int, sortSetKey string, redisClient *urchin_util.RedisStorage, peerInfos *[]UrchinPeerInfo) error {
	if orderBy == "" {
		orderBy = "create_time"
	}

	sortByStr := "ASC"
	if sortBy == -1 {
		sortByStr = "DESC"
	}

	sortByPara := storagePrefixPeer + ":" + "*->" + orderBy
	sort := redis.Sort{
		By:     sortByPara,
		Offset: int64(pageIndex),
		Count:  int64(pageSize),
		Order:  sortByStr,
		Alpha:  true,
	}
	members, err := redisClient.Sort(sortSetKey, sort)
	if err != nil {
		logger.Infof("sortAndBuildResult sort err:%v", err)
		return err
	}

	for _, member := range members {
		peerInfo, err := getPeerInfoById(member, redisClient)
		if err != nil {
			logger.Infof("sortAndBuildResult get peer by id err:%v", err)
			continue
		}

		if peerInfo.Id == "" {
			continue
		}

		*peerInfos = append(*peerInfos, peerInfo)
	}

	return nil
}
