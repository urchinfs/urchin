package urchin_task

import (
	"context"
	"d7y.io/dragonfly/v2/client/config"
	"d7y.io/dragonfly/v2/client/daemon/peer"
	logger "d7y.io/dragonfly/v2/internal/dflog"
	"d7y.io/dragonfly/v2/pkg/retry"
	"d7y.io/dragonfly/v2/scheduler/resource"
	"errors"
	"fmt"
	"github.com/docker/go-connections/tlsconfig"
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	drivermysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
	"gorm.io/plugin/soft_delete"
	"moul.io/zapgorm2"
	"net/http"
	"time"
)

const (
	// DatabaseTypeMysql is database type of mysql.
	DatabaseTypeMysql = "mysql"

	// DatabaseTypeMariaDB is database type of mariadb.
	DatabaseTypeMariaDB = "mariadb"
)

const (
	// defaultMysqlDialTimeout is dial timeout of mysql.
	defaultMysqlDialTimeout = 1 * time.Minute

	// defaultMysqlReadTimeout is I/O read timeout of mysql.
	defaultMysqlReadTimeout = 2 * time.Minute

	// defaultMysqlWriteTimeout is I/O write timeout of mysql.
	defaultMysqlWriteTimeout = 2 * time.Minute
)

type Database struct {
	DB *gorm.DB
}

func newDatabase(databaseConfig *config.DatabaseConfig) (*Database, error) {
	var (
		db  *gorm.DB
		err error
	)
	switch databaseConfig.Type {
	case DatabaseTypeMysql, DatabaseTypeMariaDB:
		db, err = newMyqsl(databaseConfig)
		if err != nil {
			logger.Errorf("mysql: %s", err.Error())
			return nil, err
		}
	default:
		return nil, fmt.Errorf("invalid urchin database type %s", databaseConfig.Type)
	}

	return &Database{
		DB: db,
	}, nil
}

func newMyqsl(databaseConfig *config.DatabaseConfig) (*gorm.DB, error) {
	mysqlCfg := &databaseConfig.Mysql

	// Format dsn string.
	dsn, err := formatMysqlDSN(mysqlCfg)
	if err != nil {
		return nil, err
	}

	// Initialize gorm logger.
	logLevel := gormlogger.Info

	gormLogger := zapgorm2.New(logger.CoreLogger.Desugar()).LogMode(logLevel)

	// Connect to mysql.
	db, err := gorm.Open(drivermysql.Open(dsn), &gorm.Config{
		NamingStrategy: schema.NamingStrategy{
			SingularTable: true,
		},
		DisableForeignKeyConstraintWhenMigrating: true,
		Logger:                                   gormLogger,
	})
	if err != nil {
		return nil, err
	}

	// Run migration.
	if mysqlCfg.Migrate {
		if err := migrate(db); err != nil {
			return nil, err
		}
	}

	return db, nil
}

type BaseModel struct {
	ID        uint                  `gorm:"primarykey;comment:id" json:"id"`
	CreatedAt time.Time             `gorm:"column:created_at;type:timestamp;default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time             `gorm:"column:updated_at;type:timestamp;default:current_timestamp" json:"updated_at"`
	IsDel     soft_delete.DeletedAt `gorm:"softDelete:flag;comment:soft delete flag" json:"-"`
}

type UrchinTask struct {
	BaseModel
	TaskID          string `gorm:"column:task_id;type:varchar(256);index:task_id;not null;comment:task id;" json:"task_id"`
	ContentLength   uint64 `gorm:"column:content_length;comment:content length" json:"content_length"`
	DataSource      string `gorm:"column:data_source;type:varchar(256);index:data_source;not null;comment:data source" json:"data_source"`
	DataDestination string `gorm:"column:data_destination;type:varchar(256);index:data_destination;not null;comment:data destination" json:"data_destination"`
	Object          string `gorm:"column:object;type:varchar(256);not null;comment:object" json:"object"`
	State           string `gorm:"column:state;type:varchar(256);index:state;not null;default:'Pending';comment:task state" json:"state"`
}

func migrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&UrchinTask{},
	)
}

func formatMysqlDSN(cfg *config.MysqlConfig) (string, error) {
	mysqlCfg := mysql.Config{
		User:                 cfg.User,
		Passwd:               cfg.Password,
		Addr:                 fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Net:                  "tcp",
		DBName:               cfg.DBName,
		Loc:                  time.Local,
		AllowNativePasswords: true,
		ParseTime:            true,
		InterpolateParams:    true,
		Timeout:              defaultMysqlDialTimeout,
		ReadTimeout:          defaultMysqlReadTimeout,
		WriteTimeout:         defaultMysqlWriteTimeout,
	}

	// Support TLS connection.
	if cfg.TLS != nil {
		mysqlCfg.TLSConfig = "custom"
		tls, err := tlsconfig.Client(tlsconfig.Options{
			CAFile:             cfg.TLS.CA,
			CertFile:           cfg.TLS.Cert,
			KeyFile:            cfg.TLS.Key,
			InsecureSkipVerify: cfg.TLS.InsecureSkipVerify,
		})
		if err != nil {
			return "", err
		}

		if err := mysql.RegisterTLSConfig("custom", tls); err != nil {
			return "", err
		}
	} else if cfg.TLSConfig != "" { // If no custom config is specified, use tlsConfig parameter if it is set.
		mysqlCfg.TLSConfig = cfg.TLSConfig
	}

	return mysqlCfg.FormatDSN(), nil
}

type UrchinTaskManager struct {
	config          *config.DaemonOption
	db              *Database
	peerTaskManager peer.TaskManager
}

func NewUrchinTaskManager(cfg *config.DaemonOption, peerTaskManager peer.TaskManager) (*UrchinTaskManager, error) {

	logger.Infof("UrchinTaskManager database config:%v", cfg.Database)
	// Initialize database
	db, err := newDatabase(&cfg.Database)
	if err != nil {
		return nil, err
	}

	return &UrchinTaskManager{
		config:          cfg,
		db:              db,
		peerTaskManager: peerTaskManager,
	}, nil
}

func (urtm *UrchinTaskManager) CreateTask(taskId string, source string, destination string, contentLength uint64, object string) error {

	task := UrchinTask{
		TaskID:          taskId,
		ContentLength:   contentLength,
		DataSource:      source,
		DataDestination: destination,
		Object:          object,
		State:           resource.TaskStatePending,
	}

	logger.Infof("Create Urchin Task:%v", task)

	if err := urtm.db.DB.WithContext(context.TODO()).Create(&task).Error; err != nil {
		return err
	}

	logger.Infof("Create polling Urchin Task routine")
	go urtm.pollingTaskState(context.Background(), taskId)

	return nil
}

func (urtm *UrchinTaskManager) pollingTaskState(ctx context.Context, taskId string) {
	var (
		task UrchinTask
	)
	if _, _, err := retry.Run(ctx, 5, 10, 480, func() (any, bool, error) {

		// Check scheduler if other peers hold the task
		taskFromScheduler, err := urtm.peerTaskManager.StatTask(ctx, taskId)
		if err != nil {
			logger.Errorf("pollingTaskState: get task From Scheduler failed: %s", err.Error())
			return nil, true, err
		}

		logger.Infof("pollingTaskState: get task From Scheduler :%v", taskFromScheduler)

		if err := urtm.db.DB.WithContext(ctx).Where("task_id=?", taskId).Order("created_at desc").First(&task).Update("state", taskFromScheduler.State).Error; err != nil {
			logger.Errorf("pollingTaskState: update urchin task state to db failed: %s", err.Error())
			return nil, true, err
		}

		switch taskFromScheduler.State {
		case resource.TaskStateSucceeded:
			logger.Info("pollingTaskState: task From Scheduler state is succeeded")
			return nil, true, nil
		case resource.TaskStateFailed:
			logger.Error("pollingTaskState: task From Scheduler state is failed")
			return nil, true, nil
		default:
			//Do not cancel to poll task state
			msg := fmt.Sprintf("pollingTaskState: task From Scheduler state is %s", taskFromScheduler.State)
			logger.Info(msg)
			return nil, false, errors.New(msg)
		}

	}); err != nil {
		logger.Errorf("pollingTaskState failed: %s", err.Error())
	}

	// Polling timeout and failed.
	if task.State != resource.TaskStateSucceeded && task.State != resource.TaskStateFailed {
		if err := urtm.db.DB.WithContext(ctx).Where("task_id=?", taskId).Order("created_at desc").First(&task).Update("state", resource.TaskStateFailed).Error; err != nil {
			logger.Errorf("pollingTaskState: update urchin task state to db failed: %s", err.Error())
		}

		logger.Error("pollingTaskState failed: timeout")
	}
}

func (urtm *UrchinTaskManager) destroyTask(ctx context.Context, taskId string) error {
	task := UrchinTask{}
	if err := urtm.db.DB.WithContext(ctx).First(&task, taskId).Error; err != nil {
		return err
	}

	if err := urtm.db.DB.WithContext(ctx).Delete(&UrchinTask{}, taskId).Error; err != nil {
		return err
	}

	return nil
}

func (urtm *UrchinTaskManager) GetTask(ctx *gin.Context) {
	var taskParams UrchinTaskParams
	if err := ctx.ShouldBindUri(&taskParams); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	task := UrchinTask{}
	if err := urtm.db.DB.WithContext(ctx).Where("task_id=?", taskParams.TaskID).Order("created_at desc").First(&task).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"errors": err.Error()})
		return
	}

	logger.Infof("Get Urchin Task from db:%v", task)

	// Task available for download only if task is in succeeded state and has available peer
	ctx.JSON(http.StatusOK, gin.H{
		"status_code":           0,
		"status_msg":            "",
		"task_id":               task.TaskID,
		"task_state":            task.State,
		"task_content_length":   task.ContentLength,
		"task_data_source":      task.DataSource,
		"task_data_destination": task.DataDestination,
	})
	return
}

type GetTasksQuery struct {
	TaskID          string `form:"task_id" binding:"omitempty"`
	DataSource      string `form:"data_source" binding:"omitempty"`
	DataDestination string `form:"data_destination" binding:"omitempty"`
	State           string `form:"state" binding:"omitempty,oneof=Pending Running Leave Succeeded Failed"`
	Page            int    `form:"page,default=0" binding:"omitempty,gte=1"`
	PerPage         int    `form:"per_page,default=1" binding:"omitempty,gte=1,lte=50"`
}

func (urtm *UrchinTaskManager) setPaginationDefault(page, perPage *int) {
	if *page == 0 {
		*page = 1
	}

	if *perPage == 0 {
		*perPage = 10
	}
}

func (urtm *UrchinTaskManager) paginate(page, perPage int) func(db *gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		offset := (page - 1) * perPage
		return db.Offset(offset).Limit(perPage)
	}
}

func (urtm *UrchinTaskManager) getUrchinTasks(ctx context.Context, q GetTasksQuery) ([]UrchinTask, int64, error) {
	var count int64
	var tasks []UrchinTask
	if err := urtm.db.DB.WithContext(ctx).Scopes(urtm.paginate(q.Page, q.PerPage)).Where(&UrchinTask{
		TaskID:          q.TaskID,
		State:           q.State,
		DataSource:      q.DataSource,
		DataDestination: q.DataDestination,
	}).Find(&tasks).Limit(-1).Offset(-1).Count(&count).Error; err != nil {
		return nil, 0, err
	}

	return tasks, count, nil
}

// @Summary Get Urchin Tasks
// @Description Get Urchin Tasks
// @Tags Task
// @Accept json
// @Produce json
// @Param page query int true "current page" default(0)
// @Param per_page query int true "return max item count, default 10, max 50" default(10) minimum(2) maximum(50)
// @Success 200 {object} []UrchinTask
// @Failure 400
// @Failure 404
// @Failure 500
// @Router /tasks [get]
func (urtm *UrchinTaskManager) GetTasks(ctx *gin.Context) {
	var query GetTasksQuery
	if err := ctx.ShouldBindQuery(&query); err != nil {
		ctx.JSON(http.StatusUnprocessableEntity, gin.H{"errors": err.Error()})
		return
	}

	urtm.setPaginationDefault(&query.Page, &query.PerPage)
	tasks, count, err := urtm.getUrchinTasks(ctx.Request.Context(), query)
	if err != nil {
		ctx.Error(err) // nolint: errcheck
		return
	}

	//h.setPaginationLinkHeader(ctx, query.Page, query.PerPage, int(count))
	totalCount := int(count)
	perPage := query.PerPage
	totalPage := totalCount / perPage
	if totalCount%perPage > 0 {
		totalPage++
	}

	ctx.JSON(http.StatusOK, gin.H{
		"status_code": 0,
		"status_msg":  "",
		"tasks":       tasks,
		"total_page":  totalPage,
	})

	return
}
