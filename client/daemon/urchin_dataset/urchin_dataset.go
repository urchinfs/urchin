package urchin_dataset

import (
	"context"
	"crypto/md5"
	"d7y.io/dragonfly/v2/client/config"
	"d7y.io/dragonfly/v2/client/daemon/urchin_dataset_vesion"
	"d7y.io/dragonfly/v2/client/urchin_util"
	logger "d7y.io/dragonfly/v2/internal/dflog"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ReplicaNoScale = iota
	ReplicaScaleUP
	ReplicaScaleDown
	ReplicaScaleUnknown
)

var conf *ConfInfo
var once sync.Once

type ConfInfo struct {
	Opt       *config.DaemonOption
	DynConfig config.Dynconfig
}

func SetDataSetConfInfo(opt *config.DaemonOption, dynConfig config.Dynconfig) {
	once.Do(func() {
		conf = &ConfInfo{
			Opt:       opt,
			DynConfig: dynConfig,
		}
	})
}

func getConfInfo() ConfInfo {
	return *conf
}

func validateReplica(wantedReplicas uint) error {

	dataSourcesInfo := getConfInfo().Opt.ObjectStorage

	if int(wantedReplicas) > dataSourcesInfo.MaxReplicas {
		return errors.New("wanted replicas: " + strconv.FormatUint(uint64(wantedReplicas), 10) + " is large than the max datasource count of system setting: " + strconv.FormatInt(int64(dataSourcesInfo.MaxReplicas), 10))
	}

	replicableDataSources, err := urchin_util.GetReplicableDataSources(getConfInfo().DynConfig, getConfInfo().Opt.Host.AdvertiseIP.String())
	if err != nil {
		return err
	}

	replicableDataSourceCnt := uint(len(replicableDataSources))
	if wantedReplicas > replicableDataSourceCnt {
		return errors.New("wanted replicas: " + strconv.FormatUint(uint64(wantedReplicas), 10) + " is large than replicable datasource count: " + strconv.FormatUint(uint64(replicableDataSourceCnt), 10))
	}

	return nil
}

// CreateDataSet POST /api/v1/dataset
func CreateDataSet(ctx *gin.Context) {
	var form UrchinDataSetCreateParams
	if err := ctx.ShouldBind(&form); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var (
		dataSetName   = form.Name
		dataSetDesc   = form.Desc
		replica       = form.Replica
		cacheStrategy = form.CacheStrategy
		dataSetTags   = form.Tags
	)

	if err := validateReplica(replica); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	dataSetID := GetUUID()
	redisClient := urchin_util.NewRedisStorage(urchin_util.RedisClusterIP, urchin_util.RedisClusterPwd, false)
	datasetKey := redisClient.MakeStorageKey([]string{dataSetID}, StoragePrefixDataset)
	values := make(map[string]interface{})
	values["id"] = dataSetID
	values["name"] = dataSetName
	values["desc"] = dataSetDesc
	if replica <= 0 {
		values["replica"] = 1
	} else {
		values["replica"] = replica
	}
	values["cache_strategy"] = cacheStrategy
	values["tags"] = strings.Join(dataSetTags, "_")
	values["share_blob_sources"] = "[]"
	values["share_blob_caches"] = "[]"
	values["replica_state"] = ReplicaNoScale

	curTime := time.Now().Unix()
	values["create_time"] = strconv.FormatInt(curTime, 10)
	values["update_time"] = strconv.FormatInt(curTime, 10)
	err := redisClient.SetMapElements(datasetKey, values)
	if err != nil {
		logger.Warnf("CreateDataSet set map elements err:%v, dataSetID:%s", err, dataSetID)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	err = redisClient.ZAdd(DatasetCreateTimeKey, dataSetID, float64(curTime))
	if err != nil {
		logger.Warnf("CreateDataSet zadd element to que  err:%v, dataSetID:%s", err, dataSetID)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	if len(dataSetName) > 0 {
		datasetNameKey := redisClient.MakeStorageKey([]string{dataSetID, "match_prefix_name", dataSetName}, StoragePrefixDataset)
		err = redisClient.Set(datasetNameKey, []byte(dataSetName))
		if err != nil {
			logger.Warnf("CreateDataSet set dataset name err:%v, dataSetID:%s", err, dataSetID)
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}
	}

	if len(dataSetTags) > 0 {
		formatTags := strings.Join(dataSetTags, "_")
		datasetTagsKey := redisClient.MakeStorageKey([]string{dataSetID, "match_prefix_tags", formatTags}, StoragePrefixDataset)
		err = redisClient.Set(datasetTagsKey, []byte(formatTags))
		if err != nil {
			logger.Warnf("CreateDataSet set dataset tags err:%v, dataSetID:%s", err, dataSetID)
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}
	}

	err = urchin_dataset_vesion.CreateDataSetVersionImpl(dataSetID, urchin_dataset_vesion.UrchinDataSetVersionInfo{
		ID:       DefaultDatasetVersion,
		Name:     "default dataset version",
		CreateAt: curTime,
	})

	if err != nil {
		logger.Warnf("create Default dataset version err:%v, dataSetID:%s", err, dataSetID)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": fmt.Sprintf("create Default dataset version err:%v, dataSetID:%s", err.Error(), dataSetID)})
		return
	}

	ctx.JSON(http.StatusOK, gin.H{
		"status_code": 0,
		"status_msg":  "succeed",
		"dataset_id":  dataSetID,
	})
	return
}

// UpdateDataSet PATCH /api/v1/dataset/:datasetid
func UpdateDataSet(ctx *gin.Context) {
	var params UrchinDataSetParams
	if err := ctx.ShouldBindUri(&params); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var form UrchinDataSetUpdateParams
	if err := ctx.ShouldBind(&form); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var (
		dataSetID     = params.ID
		dataSetName   = form.Name
		dataSetDesc   = form.Desc
		replica       = form.Replica
		cacheStrategy = form.CacheStrategy
		dataSetTags   = form.Tags
	)

	err := UpdateDataSetImpl(dataSetID, dataSetName, dataSetDesc, replica, cacheStrategy, dataSetTags, []UrchinEndpoint{}, []UrchinEndpoint{})
	if err != nil {
		logger.Warnf("UpdateDataSet err:%v, dataSetID:%s, dataSetDesc:%s", err, dataSetID, dataSetDesc)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	ctx.JSON(http.StatusOK, gin.H{
		"status_code": 0,
		"status_msg":  "succeed",
	})
	return
}

// GetDataSet GET /api/v1/dataset/:datasetid
func GetDataSet(ctx *gin.Context) {
	var params UrchinDataSetParams
	if err := ctx.ShouldBindUri(&params); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var (
		dataSetID = params.ID
	)

	dataset, err := GetDataSetImpl(dataSetID)
	if err != nil {
		logger.Warnf("GetDataSet fail, err:%v, dataSetID:%s", err, dataSetID)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	ctx.JSON(http.StatusOK, gin.H{
		"status_code": 0,
		"status_msg":  "succeed",
		"dataset":     dataset,
	})
	return
}

// ListDataSets GET /api/v1/datasets
func ListDataSets(ctx *gin.Context) {
	var form UrchinDataSetQueryParams
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

	getCacheSortSet := func() string {
		formPara := searchKey + orderBy + fmt.Sprint(sortBy) + fmt.Sprint(createdAtLess) + fmt.Sprint(createdAtGreater)
		h := md5.New()
		h.Write([]byte(formPara))

		curTime := time.Now()
		return DataSetTmpSortSet + "_" + hex.EncodeToString(h.Sum(nil)) + "_" + fmt.Sprint(curTime.Unix()-int64(curTime.Second()%20))
	}

	var datasets []UrchinDataSetInfo
	redisClient := urchin_util.NewRedisStorage(urchin_util.RedisClusterIP, urchin_util.RedisClusterPwd, false)
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
				members, err = redisClient.ZRangeByScore(DatasetCreateTimeKey, strconv.FormatInt(rangeLower, 10), strconv.FormatInt(rangeUpper, 10), int64(pageIndex), int64(pageSize))
			} else {
				members, err = redisClient.ZRevRangeByScore(DatasetCreateTimeKey, strconv.FormatInt(rangeLower, 10), strconv.FormatInt(rangeUpper, 10), int64(pageIndex), int64(pageSize))
			}
			if err != nil {
				logger.Warnf("ListDataSets range by score err:%v", err)
				ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
				return
			}

			for _, member := range members {
				dataset, err := getDataSetById(member, redisClient)
				if err != nil {
					logger.Warnf("ListDataSets get dataset by id err:%v", err)
					ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
					return
				}

				datasets = append(datasets, dataset)
			}
		} else {
			var tmpSortSetKey string
			if createdAtLess != 0 || createdAtGreater != 0 {
				tmpSortSetKey = redisClient.MakeStorageKey([]string{getCacheSortSet()}, StoragePrefixDataset)
				exists, err := redisClient.Exists(tmpSortSetKey)
				if err != nil || !exists {
					var members []string
					err := MatchZSetMemberByCreateTime(createdAtLess, createdAtGreater, DatasetCreateTimeKey, &members, redisClient)
					if err != nil {
						logger.Warnf("ListDataSets match dataset by create time err:%v", err)
						ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
						return
					}

					tmpSortSetKey = redisClient.MakeStorageKey([]string{getCacheSortSet()}, StoragePrefixDataset)
					err = WriteToTmpSet(members, tmpSortSetKey, redisClient)
					if err != nil {
						logger.Warnf("ListDataSets write to tmp set err:%v", err)
						ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
						return
					}
				}

			} else {
				tmpSortSetKey = DatasetCreateTimeKey
			}

			err := sortAndBuildResult(orderBy, sortBy, pageIndex, pageSize, tmpSortSetKey, redisClient, &datasets)
			if err != nil {
				logger.Warnf("ListDataSets sort and build result err:%v", err)
				ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
				return
			}
		}

	} else {
		var tmpSortSetKey string

		tmpSortSetKey = redisClient.MakeStorageKey([]string{getCacheSortSet()}, StoragePrefixDataset)
		exists, err := redisClient.Exists(tmpSortSetKey)
		if err != nil || !exists {
			matchName := make(map[string]bool)
			prefix := StoragePrefixDataset + "*" + "match_prefix_name:*" + searchKey + "*"
			err := MatchKeysByPrefix(prefix, matchName, redisClient)
			if err != nil {
				logger.Warnf("ListDataSets match dataset by name prefix err:%v, prefix:%s", err, prefix)
				ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
				return
			}

			matchTags := make(map[string]bool)
			prefix = StoragePrefixDataset + "*" + "match_prefix_tags:*" + searchKey + "*"
			err = MatchKeysByPrefix(prefix, matchTags, redisClient)
			if err != nil {
				logger.Warnf("ListDataSets match dataset by tags prefix err:%v, prefix:%s", err, prefix)
				ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
				return
			}

			var matchResult []string
			if createdAtLess != 0 || createdAtGreater != 0 {
				err = MatchZSetMemberByCreateTime(createdAtLess, createdAtGreater, DatasetCreateTimeKey, &matchResult, redisClient)
				if err != nil {
					logger.Warnf("ListDataSets match dataset by create time err:%v", err)
					ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
					return
				}
			}

			matchCreateTime := make(map[string]bool)
			for _, member := range matchResult {
				matchCreateTime[member] = true
			}

			matchMap := unionMap(matchName, matchTags)
			if createdAtLess != 0 || createdAtGreater != 0 {
				matchMap = InterMap(matchMap, matchCreateTime)
			}

			matchSlice := MapToSlice(matchMap)
			tmpSortSetKey = redisClient.MakeStorageKey([]string{getCacheSortSet()}, StoragePrefixDataset)
			err = WriteToTmpSet(matchSlice, tmpSortSetKey, redisClient)
			if err != nil {
				logger.Warnf("ListDataSets write to tmp set err:%v", err)
				ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
				return
			}
		}

		err = sortAndBuildResult(orderBy, sortBy, pageIndex, pageSize, tmpSortSetKey, redisClient, &datasets)
		if err != nil {
			logger.Warnf("ListDataSets sort and build result err:%v", err)
			ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
			return
		}
	}

	ctx.JSON(http.StatusOK, gin.H{
		"status_code": 0,
		"status_msg":  "succeed",
		"datasets":    datasets,
	})
	return
}

// DeleteDataSet DELETE /api/v1/dataset/:datasetid
func DeleteDataSet(ctx *gin.Context) {
	var params UrchinDataSetParams
	if err := ctx.ShouldBindUri(&params); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	var (
		dataSetID = params.ID
	)

	redisClient := urchin_util.NewRedisStorage(urchin_util.RedisClusterIP, urchin_util.RedisClusterPwd, false)
	datasetKey := redisClient.MakeStorageKey([]string{dataSetID}, StoragePrefixDataset)

	dataSetName, err := redisClient.GetMapElement(datasetKey, "name")
	if err != nil {
		logger.Warnf("DeleteDataSet get map element name err:%v, dataSetID:%s", err, dataSetID)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	if len(dataSetName) > 0 {
		datasetNameKey := redisClient.MakeStorageKey([]string{dataSetID, "match_prefix_name", dataSetName}, StoragePrefixDataset)
		err := redisClient.Del(datasetNameKey)
		if err != nil {
			logger.Warnf("DeleteDataSet del key %s err:%v, dataSetID:%s", datasetNameKey, err)
		}
	}

	dataSetTags, err := redisClient.GetMapElement(datasetKey, "tags")
	if err != nil {
		logger.Warnf("DeleteDataSet get map element tags err:%v", err, dataSetID)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	if len(dataSetTags) > 0 {
		datasetTagsKey := redisClient.MakeStorageKey([]string{dataSetID, "match_prefix_tags", dataSetTags}, StoragePrefixDataset)
		err := redisClient.Del(datasetTagsKey)
		if err != nil {
			logger.Warnf("DeleteDataSet del key %s err:%v", datasetTagsKey, err)
		}
	}

	err = redisClient.ZRem(DatasetCreateTimeKey, dataSetID)
	if err != nil {
		logger.Warnf("DeleteDataSet zRem key %s err:%v", dataSetID, err)
	}

	err = redisClient.DeleteMap(datasetKey)
	if err != nil {
		logger.Warnf("DeleteDataSet del map err:%v, dataSetID:%s", err, dataSetID)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}
	err = redisClient.Del(datasetKey)
	if err != nil {
		logger.Warnf("DeleteDataSet del map key err:%v, dataSetID:%s", err, dataSetID)
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	ctx.JSON(http.StatusOK, gin.H{
		"status_code": 0,
		"status_msg":  "succeed",
	})
	return
}

func unionMap(m1, m2 map[string]bool) map[string]bool {
	result := make(map[string]bool)
	for k, v := range m1 {
		result[k] = v
	}
	for k, v := range m2 {
		if _, ok := result[k]; !ok {
			result[k] = v
		}
	}
	return result
}

func InterMap(m1, m2 map[string]bool) map[string]bool {
	result := make(map[string]bool)
	for k, v := range m1 {
		if _, ok := m2[k]; ok {
			result[k] = v
		}
	}
	return result
}

func MapToSlice(m map[string]bool) []string {
	s := make([]string, 0, len(m))
	for k := range m {
		s = append(s, k)
	}
	return s
}

func UpdateDataSetImpl(dataSetID, dataSetName string, dataSetDesc string, wantedReplica uint, cacheStrategy string, dataSetTags []string,
	shareBlobSources, shareBlobCaches []UrchinEndpoint) error {
	logger.Infof("updateDataSet dataSetID:%s,name:%s desc:%s replica:%d cacheStrategy:%s tags:%v shareBlobSources:%v shareBlobCaches:%v",
		dataSetID, dataSetName, dataSetDesc, wantedReplica, cacheStrategy, dataSetTags, shareBlobSources, shareBlobCaches)

	oldDatasetInfo, err := GetDataSetImpl(dataSetID)
	if err != nil {
		logger.Warnf("updateDataSet get dataSet err:%v, dataSetID:%s", err, dataSetID)
		return err
	}

	redisClient := urchin_util.NewRedisStorage(urchin_util.RedisClusterIP, urchin_util.RedisClusterPwd, false)
	datasetKey := redisClient.MakeStorageKey([]string{dataSetID}, StoragePrefixDataset)
	updateDataSetFunc := func() error {
		if len(dataSetName) > 0 {
			err := redisClient.SetMapElement(datasetKey, "name", []byte(dataSetName))
			if err != nil {
				logger.Warnf("updateDataSet set map element err:%v, dataSetID:%s, name:%s", err, dataSetID, dataSetName)
				return err
			}
		}

		if len(dataSetDesc) > 0 {
			err := redisClient.SetMapElement(datasetKey, "desc", []byte(dataSetDesc))
			if err != nil {
				logger.Warnf("updateDataSet set map element err:%v, dataSetID:%s, desc:%s", err, dataSetID, dataSetDesc)
				return err
			}
		}
		if wantedReplica > 0 {
			err := redisClient.SetMapElement(datasetKey, "replica", []byte(strconv.FormatInt(int64(wantedReplica), 10)))
			if err != nil {
				logger.Warnf("updateDataSet set map element err:%v, dataSetID:%s, replica:%d", err, dataSetID, wantedReplica)
				return err
			}
		}
		if len(cacheStrategy) > 0 {
			err := redisClient.SetMapElement(datasetKey, "cache_strategy", []byte(cacheStrategy))
			if err != nil {
				logger.Warnf("updateDataSet set map element err:%v, dataSetID:%s, cache_strategy:%d", err, dataSetID, cacheStrategy)
				return err
			}
		}
		if len(dataSetTags) > 0 {
			oldTags, err := redisClient.GetMapElement(datasetKey, "tags")
			if err != nil {
				logger.Warnf("updateDataSet get map old element err:%v, dataSetID:%s, tags:%d", err, dataSetID, dataSetTags)
				return err
			}

			oldTagsKey := redisClient.MakeStorageKey([]string{dataSetID, "match_prefix_tags", oldTags}, StoragePrefixDataset)
			_ = redisClient.Del(oldTagsKey)

			err = redisClient.SetMapElement(datasetKey, "tags", []byte(strings.Join(dataSetTags, "_")))
			if err != nil {
				logger.Warnf("updateDataSet set map element err:%v, dataSetID:%s, tags:%d", err, dataSetID, dataSetTags)
				return err
			}

			formatTags := strings.Join(dataSetTags, "_")
			datasetTagsKey := redisClient.MakeStorageKey([]string{dataSetID, "match_prefix_tags", formatTags}, StoragePrefixDataset)
			err = redisClient.Set(datasetTagsKey, []byte(formatTags))
			if err != nil {
				logger.Warnf("updateDataSet set dataset tags err:%v, dataSetID:%s", err, dataSetID)
				return err
			}
		}

		if len(shareBlobSources) > 0 {
			jsonBody, err := json.Marshal(shareBlobSources)
			if err != nil {
				logger.Warnf("updateDataSet json marshal err:%v, dataSetID:%s, shareBlobSources:%d", err, dataSetID, shareBlobSources)
				return err
			}
			err = redisClient.SetMapElement(datasetKey, "share_blob_sources", jsonBody)
			if err != nil {
				logger.Warnf("updateDataSet set map element err:%v, dataSetID:%s, shareBlobSources:%d", err, dataSetID, shareBlobSources)
				return err
			}
		}

		if len(shareBlobCaches) > 0 {
			jsonBody, err := json.Marshal(shareBlobCaches)
			if err != nil {
				logger.Warnf("updateDataSet json marshal err:%v, dataSetID:%s, shareBlobCaches:%d", err, dataSetID, shareBlobCaches)
				return err
			}
			err = redisClient.SetMapElement(datasetKey, "share_blob_caches", jsonBody)
			if err != nil {
				logger.Warnf("updateDataSet set map element err:%v, dataSetID:%s, shareBlobCaches:%d", err, dataSetID, shareBlobCaches)
				return err
			}
		}

		curTime := time.Now().Unix()
		_ = redisClient.SetMapElement(datasetKey, "update_time", []byte(strconv.FormatInt(curTime, 10)))

		logger.Infof("updateDataSet dataSetID:%s complete", dataSetID)
		return nil
	}

	if wantedReplica > 0 && oldDatasetInfo.Replica != wantedReplica {
		logger.Infof("updateDataSet dataSetID:%s need adjust replica:%d num to:%d", dataSetID, oldDatasetInfo.Replica, wantedReplica)

		if len(oldDatasetInfo.ShareBlobSources) < 1 {
			logger.Errorf("dataset:%s share blob sources is valid", dataSetID)
			return errors.New("internal error: share blob sources is valid")
		}
		sourceEndpoint := oldDatasetInfo.ShareBlobSources[0].Endpoint
		sourceEndpointPath := oldDatasetInfo.ShareBlobSources[0].EndpointPath
		sourceBucketObject := strings.SplitN(sourceEndpointPath, ".", 2)
		if len(sourceBucketObject) < 2 {
			logger.Errorf("share blob sources bucket %v is invalid", sourceBucketObject)
			return errors.New("internal error: share blob sources bucket is valid")
		}

		if wantedReplica < oldDatasetInfo.Replica {
			err := setReplicaState(dataSetID, ReplicaScaleDown)
			if err != nil {
				logger.Warnf("set replica state:%d failed, dataSetID:%s, error:%v", ReplicaScaleDown, dataSetID, err)
				return err
			}

			defer func(dataSetID string, state uint) {
				err := setReplicaState(dataSetID, state)
				if err != nil {
					logger.Warnf("set replica state:%d failed, dataSetID:%s, error:%v", state, dataSetID, err)
				}
			}(dataSetID, ReplicaNoScale)

			ScaleDownReplicaHosts, err := selectScaleDownReplicaHosts(dataSetID, wantedReplica, redisClient)
			if err != nil {
				logger.Errorf("selectScaleDownReplicaHosts failed,  dataset:%s error:%s", dataSetID, err)
				return err
			}

			shareBlobCaches = oldDatasetInfo.ShareBlobCaches[0:wantedReplica]
			err = updateDataSetFunc()
			if err != nil {
				logger.Errorf("update dataset:%s info error:%s", dataSetID, err)
				return err
			}

			err = scaleDownDatasetVersionInfo(dataSetID, wantedReplica)
			if err != nil {
				logger.Warnf("dataset:%s scale down dataset version info error:%s", dataSetID, err)
				return err
			}

			logger.Infof("dataset:%s scale down dataset host:%v", dataSetID, ScaleDownReplicaHosts)
			for _, replicaHost := range ScaleDownReplicaHosts {
				err := destroySeedPeerDataset(context.Background(), dataSetID, replicaHost, sourceBucketObject[0], sourceBucketObject[1])
				if err != nil {
					logger.Warnf("destroySeedPeerDataset scale down replica host failed, dataSetID:%s, error:%v", dataSetID, err)
					continue
				}
			}
			logger.Infof("dataset:%s scale down dataset finish", dataSetID)

		} else {
			err := setReplicaState(dataSetID, ReplicaScaleUP)
			if err != nil {
				logger.Warnf("set replica state:%d failed, dataSetID:%s, error:%v", ReplicaScaleUP, dataSetID, err)
				return err
			}

			replicaHosts, scaleUpReplicas, err := selectScaleUpReplicaHosts(dataSetID, wantedReplica, oldDatasetInfo.Replica)
			if err != nil {
				err = setReplicaState(dataSetID, ReplicaNoScale)
				if err != nil {
					logger.Warnf("set replica state:%d failed, dataSetID:%s, error:%v", ReplicaNoScale, dataSetID, err)
				}

				logger.Warnf("selectScaleUpReplicaHosts select replica hosts failed, dataSetID:%s, error:%v", dataSetID, err)
				return err
			}

			go func() {
				defer func(dataSetID string, state uint) {
					err := setReplicaState(dataSetID, state)
					if err != nil {
						logger.Warnf("selectScaleUpReplicaHosts set replica state:%d failed, dataSetID:%s, error:%v", state, dataSetID, err)
					}
				}(dataSetID, ReplicaNoScale)

				var scaleUpCachesEndpoint []UrchinEndpoint
				for _, scaleUpReplica := range scaleUpReplicas {
					var urchinEndpoint *UrchinEndpoint
					urchinEndpoint, err = scaleUpSeedPeerDataset(context.Background(), scaleUpReplica, sourceBucketObject[0]+"."+sourceEndpoint, sourceBucketObject[1])
					if err != nil {
						time.Sleep(time.Second * 5)
						urchinEndpoint, err = scaleUpSeedPeerDataset(context.Background(), scaleUpReplica, sourceBucketObject[0]+"."+sourceEndpoint, sourceBucketObject[1])
						if err != nil {
							logger.Warnf("scale up seed peer object error:%s, dataset:%s scale host info:%s:%S:%s", err, dataSetID, scaleUpReplica, sourceBucketObject[0]+"."+sourceEndpoint, sourceBucketObject[1])
							return
						}
					}

					scaleUpCachesEndpoint = append(scaleUpCachesEndpoint, *urchinEndpoint)
				}

				shareBlobCaches = oldDatasetInfo.ShareBlobCaches
				shareBlobCaches = append(shareBlobCaches, scaleUpCachesEndpoint...)
				err = updateDataSetFunc()
				if err != nil {
					logger.Warnf("dataset:%s update dataset info error:%s", dataSetID, err)
					return
				}

				newReplicaHosts := append(replicaHosts, scaleUpReplicas...)
				err = updateRedisReplicaInfo(dataSetID, newReplicaHosts, redisClient)
				if err != nil {
					logger.Warnf("dataset:%s update redis replica info error:%s", dataSetID, err)
					return
				}

				err = scaleUpDatasetVersionInfo(dataSetID, scaleUpCachesEndpoint)
				if err != nil {
					logger.Warnf("dataset:%s scale up dataset version info error:%s", dataSetID, err)
					return
				}

				logger.Infof("dataset:%s scale up dataset finish", dataSetID)
			}()

		}

		return nil
	}

	err = updateDataSetFunc()
	if err != nil {
		logger.Errorf("update dataset:%s info error:%s", dataSetID, err)
		return err
	}

	return nil
}

func GetDataSetImpl(dataSetID string) (UrchinDataSetInfo, error) {
	if dataSetID == "" {
		return UrchinDataSetInfo{}, fmt.Errorf("dataSet ID is empty")
	}

	redisClient := urchin_util.NewRedisStorage(urchin_util.RedisClusterIP, urchin_util.RedisClusterPwd, false)
	datasetKey := redisClient.MakeStorageKey([]string{dataSetID}, StoragePrefixDataset)
	elements, err := redisClient.ReadMap(datasetKey)
	if err != nil {
		logger.Warnf("GetDataSetImpl read map element err:%v, dataSetID:%s", err, dataSetID)
		return UrchinDataSetInfo{}, err
	}

	if string(elements["id"]) != dataSetID {
		logger.Warnf("GetDataSetImpl can not found dataSetID:%s", dataSetID)
		return UrchinDataSetInfo{}, err
	}

	err = nil
	var dataset UrchinDataSetInfo
	for k, v := range elements {
		if k == "tags" {
			dataset.Tags = strings.Split(string(v), "_")
		} else if k == "share_blob_sources" {
			err = json.Unmarshal(v, &dataset.ShareBlobSources)
		} else if k == "share_blob_caches" {
			err = json.Unmarshal(v, &dataset.ShareBlobCaches)
		} else if k == "id" {
			dataset.Id = string(v)
		} else if k == "name" {
			dataset.Name = string(v)
		} else if k == "desc" {
			dataset.Desc = string(v)
		} else if k == "replica" {
			var tmpReplica int
			tmpReplica, err = strconv.Atoi(string(v))
			dataset.Replica = uint(tmpReplica)
		} else if k == "cache_strategy" {
			dataset.CacheStrategy = string(v)
		}

		if err != nil {
			logger.Warnf("GetDataSetImpl json unmarshal err:%v, dataSetID:%s", err, dataSetID)
			return UrchinDataSetInfo{}, err
		}
	}

	return dataset, nil
}

func getDataSetById(dataSetID string, redisClient *urchin_util.RedisStorage) (UrchinDataSetInfo, error) {
	var dataset UrchinDataSetInfo
	datasetKey := redisClient.MakeStorageKey([]string{dataSetID}, StoragePrefixDataset)
	elements, err := redisClient.ReadMap(datasetKey)
	if err != nil {
		logger.Warnf("getDataSetById read map element err:%v, dataSetID:%s", err, dataSetID)
		return dataset, err
	}

	for k, v := range elements {
		if k == "tags" {
			dataset.Tags = strings.Split(string(v), "_")
		} else if k == "share_blob_sources" {
			err = json.Unmarshal(v, &dataset.ShareBlobSources)
		} else if k == "share_blob_caches" {
			err = json.Unmarshal(v, &dataset.ShareBlobCaches)
		} else if k == "id" {
			dataset.Id = string(v)
		} else if k == "name" {
			dataset.Name = string(v)
		} else if k == "desc" {
			dataset.Desc = string(v)
		} else if k == "replica" {
			var tmpReplica int
			tmpReplica, err = strconv.Atoi(string(v))
			dataset.Replica = uint(tmpReplica)
		} else if k == "cache_strategy" {
			dataset.CacheStrategy = string(v)
		}

		if err != nil {
			logger.Warnf("getDataSetById json unmarshal err:%v, dataSetID:%s", err, dataSetID)
			return dataset, err
		}
	}

	return dataset, nil
}

func WriteToTmpSet(members []string, tmpSortSetKey string, redisClient *urchin_util.RedisStorage) error {
	for _, member := range members {
		err := redisClient.InsertSet(tmpSortSetKey, member)
		if err != nil {
			return err
		}
	}

	_ = redisClient.SetTTL(tmpSortSetKey, time.Second*120)
	return nil
}

func MatchKeysByPrefix(prefix string, matchResult map[string]bool, redisClient *urchin_util.RedisStorage) error {
	var cursor uint64
	for {
		members, cursor, err := redisClient.Scan(cursor, prefix, 100)
		if err != nil {
			return err
		}

		for _, member := range members {
			segments := strings.Split(member, ":")
			matchResult[segments[2]] = true
		}

		if cursor == 0 {
			break
		}
	}

	return nil
}

func MatchZSetMemberByCreateTime(createdAtLess, createdAtGreater int64, zsetKey string, matchResult *[]string, redisClient *urchin_util.RedisStorage) error {
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

	var offset int64 = 0
	var count int64 = 100
	for {
		members, err := redisClient.ZRangeByScore(zsetKey, strconv.FormatInt(rangeLower, 10), strconv.FormatInt(rangeUpper, 10), offset, count)
		if err != nil {
			return err
		}

		*matchResult = append(*matchResult, members...)

		if len(members) <= 0 {
			break
		}

		offset += count
	}

	return nil
}

func sortAndBuildResult(orderBy string, sortBy int, pageIndex, pageSize int, sortSetKey string, redisClient *urchin_util.RedisStorage, datasets *[]UrchinDataSetInfo) error {
	if orderBy == "" {
		orderBy = "create_time"
	}

	sortByStr := "ASC"
	if sortBy == -1 {
		sortByStr = "DESC"
	}

	sortByPara := StoragePrefixDataset + ":" + "*->" + orderBy
	sort := redis.Sort{
		By:     sortByPara,
		Offset: int64(pageIndex),
		Count:  int64(pageSize),
		Order:  sortByStr,
		Alpha:  true,
	}
	members, err := redisClient.Sort(sortSetKey, sort)
	if err != nil {
		logger.Warnf("ListDataSets->sortAndBuildResult sort err:%v", err)
		return err
	}

	for _, member := range members {
		dataset, err := getDataSetById(member, redisClient)
		if err != nil {
			logger.Warnf("ListDataSets->sortAndBuildResult get dataset by id err:%v", err)
			continue
		}

		if dataset.Id == "" {
			continue
		}

		*datasets = append(*datasets, dataset)
	}

	return nil
}

func GetUUID() string {
	u2 := uuid.New()
	return u2.String()
}

// importObjectToSeedPeer uses to import objects to seed peer.
func destroySeedPeerDataset(ctx context.Context, dataSetID, seedPeerHost, bucketName, folderKey string) error {
	logger.Infof("destroy seedPeer host:%s dataset%s, bucketName:%s folderKey:%s", seedPeerHost, dataSetID, bucketName, folderKey)

	u := url.URL{
		Scheme: "http",
		Host:   seedPeerHost,
		Path:   filepath.Join("buckets", bucketName, "destroy_folder", folderKey),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), nil)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("bad response status %s", resp.Status)
	}

	return nil
}

func scaleUpSeedPeerDataset(ctx context.Context, seedPeerHost, bucketName, folderKey string) (*UrchinEndpoint, error) {
	u := url.URL{
		Scheme: "http",
		Host:   seedPeerHost,
		Path:   filepath.Join("buckets", bucketName, "cache_folder", folderKey),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("bad response status %s", resp.Status)
	}

	var urchinEndpoint *UrchinEndpoint
	for {
		time.Sleep(time.Second * 3)

		checkObjectStatus := func() (*UrchinEndpoint, error) {
			u := url.URL{
				Scheme: "http",
				Host:   seedPeerHost,
				Path:   filepath.Join("buckets", bucketName, "check_folder", folderKey),
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
			if err != nil {
				return nil, err
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()

			if resp.StatusCode/100 != 2 {
				time.Sleep(time.Second * 2)
				req, err = http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
				if err != nil {
					return nil, err
				}

				resp, err = http.DefaultClient.Do(req)
				if err != nil {
					return nil, err
				}

				if resp.StatusCode/100 != 2 {
					return nil, fmt.Errorf("bad response status %s", resp.Status)
				}
			}

			respBody, _ := io.ReadAll(resp.Body)
			var result map[string]any
			err = json.Unmarshal(respBody, &result)
			if err != nil {
				return nil, err
			}

			statusCode := int(result["StatusCode"].(float64))
			if statusCode == 1 {
				time.Sleep(time.Second * 20)
				return nil, nil
			}

			if statusCode != 0 {
				return nil, fmt.Errorf("bad response status %v", result["StatusCode"])
			}

			return &UrchinEndpoint{
				Endpoint:     result["DataEndpoint"].(string),
				EndpointPath: result["DataRoot"].(string) + "." + result["DataPath"].(string),
			}, nil

		}

		urchinEndpoint, err = checkObjectStatus()
		if err != nil {
			return nil, err
		}

		if urchinEndpoint == nil {
			continue
		}

		break
	}

	return urchinEndpoint, nil
}

func containsString(src []string, dest string) bool {
	for _, item := range src {
		if item == dest {
			return true
		}
	}
	return false
}

func differenceSlice(src []string, dest []string) []string {
	res := make([]string, 0)
	for _, item := range src {
		if !containsString(dest, item) {
			res = append(res, item)
		}
	}
	return res
}

func getReplicaHosts(dataSetID string) ([]string, error) {
	redisClient := urchin_util.NewRedisStorage(urchin_util.RedisClusterIP, urchin_util.RedisClusterPwd, false)
	replicaKey := redisClient.MakeStorageKey([]string{"replica", "seed-peer", dataSetID}, "")
	exists, err := redisClient.Exists(replicaKey)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.New("internal error: can not find dataset")
	}

	value, err := redisClient.Get(replicaKey)
	if err != nil {
		return nil, err
	}

	var replicaHosts []string
	err = json.Unmarshal(value, &replicaHosts)
	if err != nil {
		return nil, err
	}

	return replicaHosts, nil
}

func selectScaleUpReplicaHosts(dataSetID string, wantedReplica uint, nowReplica uint) ([]string, []string, error) {
	replicaHosts, err := getReplicaHosts(dataSetID)
	if err != nil {
		logger.Warnf("getReplicaHosts get replica host failed, dataSetID:%s, error:%v", dataSetID, err)
		return nil, nil, err
	}

	replicableDataSources, err := urchin_util.GetReplicableDataSources(getConfInfo().DynConfig, getConfInfo().Opt.Host.AdvertiseIP.String())
	if err != nil {
		logger.Warnf("get replicable data sources failed, dataSetID:%s, error:%v", dataSetID, err)
		return nil, nil, err
	}

	replicableDataSourceCnt := uint(len(replicableDataSources))
	if wantedReplica > replicableDataSourceCnt {
		logger.Warnf("dataset:%s wanted replicas:%d is large than replicable datasource count:%d", dataSetID, wantedReplica, replicableDataSourceCnt)
		return nil, nil, errors.New("wanted replicas: " + strconv.FormatUint(uint64(wantedReplica), 10) + " is large than replicable datasource count: " + strconv.FormatUint(uint64(replicableDataSourceCnt), 10))
	}

	scaleUpReplicas := differenceSlice(replicableDataSources, replicaHosts)[0 : wantedReplica-nowReplica]
	logger.Infof("get replicable data sources host:%v, dataSetID:%s", scaleUpReplicas, dataSetID)

	return replicaHosts, scaleUpReplicas, nil
}

func selectScaleDownReplicaHosts(dataSetID string, wantedReplica uint, redisClient *urchin_util.RedisStorage) ([]string, error) {
	replicaHosts, err := getReplicaHosts(dataSetID)
	if err != nil {
		logger.Warnf("getReplicaHosts get replica info dataset:%s error:%v", dataSetID, err)
		return nil, err
	}

	jsonBody, err := json.Marshal(replicaHosts[0:wantedReplica])
	if err != nil {
		logger.Warnf("json marshal failed, dataset:%s error:%v", dataSetID, err)
		return nil, err
	}

	replicaKey := redisClient.MakeStorageKey([]string{"replica", "seed-peer", dataSetID}, "")
	err = redisClient.Set(replicaKey, jsonBody)
	if err != nil {
		logger.Warnf("redis set replicaKey failed, dataset:%s jsonBody:%s error:%v", dataSetID, jsonBody, err)
		return nil, err
	}

	ScaleDownReplicaHosts := replicaHosts[wantedReplica:]

	return ScaleDownReplicaHosts, nil
}

func updateRedisReplicaInfo(dataSetID string, newReplicaHosts []string, redisClient *urchin_util.RedisStorage) error {
	jsonBody, err := json.Marshal(newReplicaHosts)
	if err != nil {
		logger.Warnf("json marshal failed, dataset:%s error:%v", dataSetID, err)
		return err
	}

	replicaKey := redisClient.MakeStorageKey([]string{"replica", "seed-peer", dataSetID}, "")
	err = redisClient.Set(replicaKey, jsonBody)
	if err != nil {
		logger.Warnf("redis set replicaKey failed, dataset:%s jsonBody:%s error:%v", dataSetID, jsonBody, err)
		return err
	}

	return nil
}

func scaleUpDatasetVersionInfo(dataSetID string, scaleUpCachesEndpoint []UrchinEndpoint) error {
	dataSetVersions, err := urchin_dataset_vesion.ListAllDataSetVersions(dataSetID)
	if err != nil || len(dataSetVersions) < 1 {
		logger.Errorf("ListAllDataSetVersions failed or dataSetVersions length equal 0, dataset:%s error:%v", dataSetID, err)
		return err
	}

	for _, versionInfo := range dataSetVersions {
		var metaCaches []UrchinEndpoint
		err = json.Unmarshal([]byte(versionInfo.MetaCaches), &metaCaches)
		if err != nil {
			logger.Errorf("json unmarshal metaCaches error, dataSetID:%s, dataSetVersion:%s, error:%v", dataSetID, versionInfo.ID, err)
			return err
		}

		var metaSources []UrchinEndpoint
		err = json.Unmarshal([]byte(versionInfo.MetaSources), &metaSources)
		if err != nil {
			logger.Errorf("json unmarshal metaSources error, dataSetID:%s, dataSetVersion:%s, error:%v", dataSetID, versionInfo.ID, err)
			return err
		}
		if len(metaSources) < 1 {
			return errors.New("dataset version meta sources is empty")
		}

		tmpScaleUpCachesEndpoint := make([]UrchinEndpoint, len(scaleUpCachesEndpoint))
		copy(tmpScaleUpCachesEndpoint, scaleUpCachesEndpoint)
		_, objectName := path.Split(metaSources[0].EndpointPath)
		for idx, _ := range tmpScaleUpCachesEndpoint {
			tmpScaleUpCachesEndpoint[idx].EndpointPath = path.Join(tmpScaleUpCachesEndpoint[idx].EndpointPath, objectName)
		}

		metaCaches = append(metaCaches, tmpScaleUpCachesEndpoint...)
		metaCacheJson, _ := json.Marshal(metaCaches)
		dataSetVersionInfo := urchin_dataset_vesion.UrchinDataSetVersionInfo{
			MetaCaches: string(metaCacheJson),
		}

		err = urchin_dataset_vesion.UpdateDataSetVersionImpl(dataSetID, versionInfo.ID, dataSetVersionInfo)
		if err != nil {
			logger.Errorf("UpdateDataSetVersionImpl error, dataSetID:%s, dataSetVersion:%s, error:%v", dataSetID, versionInfo.ID, err)
			return err
		}
	}

	return nil
}

func scaleDownDatasetVersionInfo(dataSetID string, wantedReplica uint) error {
	dataSetVersions, err := urchin_dataset_vesion.ListAllDataSetVersions(dataSetID)
	if err != nil || len(dataSetVersions) < 1 {
		logger.Errorf("ListAllDataSetVersions failed or dataSetVersions length equal 0, dataset:%s error:%v", dataSetID, err)
		return err
	}

	for _, versionInfo := range dataSetVersions {
		var metaCaches []UrchinEndpoint
		err = json.Unmarshal([]byte(versionInfo.MetaCaches), &metaCaches)
		if err != nil {
			logger.Errorf("json unmarshal error, dataSetID:%s, dataSetVersion:%s, error:%v", dataSetID, versionInfo.ID, err)
			return err
		}

		if len(metaCaches) <= 0 {
			continue
		}

		metaCaches = metaCaches[0:wantedReplica]
		metaCacheJson, _ := json.Marshal(metaCaches)
		dataSetVersionInfo := urchin_dataset_vesion.UrchinDataSetVersionInfo{
			MetaCaches: string(metaCacheJson),
		}

		err = urchin_dataset_vesion.UpdateDataSetVersionImpl(dataSetID, versionInfo.ID, dataSetVersionInfo)
		if err != nil {
			logger.Errorf("UpdateDataSetVersionImpl error, dataSetID:%s, dataSetVersion:%s, error:%v", dataSetID, versionInfo.ID, err)
			return err
		}
	}

	return nil
}

func setReplicaState(dataSetID string, state uint) error {
	redisClient := urchin_util.NewRedisStorage(urchin_util.RedisClusterIP, urchin_util.RedisClusterPwd, false)
	datasetKey := redisClient.MakeStorageKey([]string{dataSetID}, StoragePrefixDataset)

	err := redisClient.SetMapElement(datasetKey, "replica_state", []byte(strconv.FormatInt(int64(state), 10)))
	if err != nil {
		logger.Warnf("set map element err:%v, dataSetID:%s, replica_state:%d", err, dataSetID, state)
		return err
	}

	return nil
}
