package server

import (
	"fmt"
	"math"
	"sync"

	"actiontech.cloud/sqle/sqle/sqle/errors"
	"actiontech.cloud/sqle/sqle/sqle/inspector"
	"actiontech.cloud/sqle/sqle/sqle/log"
	"actiontech.cloud/sqle/sqle/sqle/model"
)

var sqled *Sqled

func GetSqled() *Sqled {
	return sqled
}

// Sqled is an async task scheduling service.
// receive tasks from queue, the tasks include inspect, commit, rollback;
// and the task will only be executed once.
type Sqled struct {
	sync.Mutex
	// exit is Sqled service exit signal.
	exit chan struct{}
	// currentTask record the current task before execution,
	// and delete it after execution.
	currentTask map[string]struct{}
	// queue is a chan used to receive tasks.
	queue chan *Action
	// instancesStatus save schemas info for all db instance.
	instancesStatus map[string]*InstanceStatus
}

// Action is an action for the task;
// when you want to commit a task, you can define an action whose type is rollback.
type Action struct {
	sync.Mutex
	Task *model.Task
	// Typ is task type, include inspect, commit, rollback.
	Typ   int
	Error error
	Done  chan struct{}
}

func InitSqled(exit chan struct{}) {
	sqled = &Sqled{
		exit:        exit,
		currentTask: map[string]struct{}{},
		queue:       make(chan *Action, 1024),
	}
	sqled.Start()
}

func (s *Sqled) HasTask(taskId string) bool {
	s.Lock()
	_, ok := s.currentTask[taskId]
	s.Unlock()
	return ok
}

// addTask receive taskId and action type, using taskId and typ to create an action;
// action will be validated, and sent to Sqled.queue.
func (s *Sqled) addTask(taskId string, typ int) (*Action, error) {
	var err error
	action := &Action{
		Typ:  typ,
		Done: make(chan struct{}),
	}
	s.Lock()
	_, taskRunning := s.currentTask[taskId]
	if !taskRunning {
		s.currentTask[taskId] = struct{}{}
	}
	s.Unlock()
	if taskRunning {
		return action, errors.New(errors.TASK_RUNNING, fmt.Errorf("task is running"))
	}

	task, exist, err := model.GetStorage().GetTaskDetailById(taskId)
	if err != nil {
		goto Error
	}
	if !exist {
		err = errors.New(errors.TASK_NOT_EXIST, fmt.Errorf("task not exist"))
		goto Error
	}

	err = task.ValidAction(typ)
	if err != nil {
		goto Error
	}

	action.Task = task
	s.queue <- action
	return action, nil

Error:
	s.Lock()
	delete(s.currentTask, taskId)
	s.Unlock()
	return action, err
}

func (s *Sqled) AddTask(taskId string, typ int) error {
	_, err := s.addTask(taskId, typ)
	return err
}

func (s *Sqled) AddTaskWaitResult(taskId string, typ int) (*model.Task, error) {
	action, err := s.addTask(taskId, typ)
	if err != nil {
		return nil, err
	}
	<-action.Done
	return action.Task, action.Error
}

func (s *Sqled) Start() {
	go s.taskLoop()
	go s.statusLoop()
	go s.cleanLoop()
}

// taskLoop is a task loop used to receive action from queue.
func (s *Sqled) taskLoop() {
	for {
		select {
		case <-s.exit:
			return
		case action := <-s.queue:
			go s.do(action)
		}
	}
}

func (s *Sqled) do(action *Action) error {
	var err error
	switch action.Typ {
	case model.TASK_ACTION_AUDIT:
		err = s.audit(action.Task)
	case model.TASK_ACTION_EXECUTE:
		err = s.commit(action.Task)
	case model.TASK_ACTION_ROLLBACK:
		err = s.rollback(action.Task)
	}
	if err != nil {
		action.Error = err
	}
	s.Lock()
	taskId := fmt.Sprintf(fmt.Sprintf("%d", action.Task.ID))
	delete(s.currentTask, taskId)
	s.Unlock()

	select {
	case action.Done <- struct{}{}:
	default:
	}
	return err
}

func (s *Sqled) audit(task *model.Task) error {
	entry := log.NewEntry().WithField("task_id", task.ID)

	st := model.GetStorage()

	rules, err := st.GetRulesByInstanceId(fmt.Sprintf("%v", task.InstanceId))
	if err != nil {
		entry.Logger.Errorf("get instance rule from storage failed, error: %v", err)
		return err
	}
	ruleMap := model.GetRuleMapFromAllArray(rules)
	ctx := inspector.NewContext(nil)
	i := inspector.NewInspector(entry, ctx, task, nil, ruleMap)
	err = i.Advise(rules)
	if err != nil {
		return err
	}
	firstSqlInvalid := i.SqlInvalid()
	sqlType := i.SqlType()
	// if sql type is DML and sql invalid, try to advise with other DDL.
	if sqlType == model.SQL_TYPE_DML && i.SqlInvalid() {
		relateTasks, err := st.GetRelatedDDLTask(task)
		if err != nil {
			return err
		}
		if len(relateTasks) > 0 {
			entry.Warnf("dml sql invalid, retry advise with relate ddl")
			i = inspector.NewInspector(entry, ctx, task, relateTasks, ruleMap)
			err = i.Advise(rules)
			if err != nil {
				return err
			}
		}
	}
	// generate rollback after advise
	var rollbackSqls = []*model.RollbackSQL{}

	if firstSqlInvalid {
		entry.Warnf("sql invalid, ignore generate rollback")
	} else if task.SQLSource == model.TaskSQLSourceFromMyBatisXMLFile {
		entry.Warnf("task is mybatis xml file audit, ignore generate rollback")
	} else {
		ctx = inspector.NewContext(nil)
		i = inspector.NewInspector(entry, ctx, task, nil, ruleMap)
		rollbackSqls, err = i.GenerateAllRollbackSql()
		if err != nil {
			return err
		}
	}

	if err := st.UpdateExecuteSQLs(task, task.ExecuteSQLs); err != nil {
		entry.Errorf("save commit sql to storage failed, error: %v", err)
		return err
	}

	var normalCount float64
	for _, sql := range task.ExecuteSQLs {
		if sql.AuditLevel == model.RULE_LEVEL_NORMAL {
			normalCount += 1
		}
	}
	if len(task.ExecuteSQLs) != 0 {
		task.PassRate = round(normalCount/float64(len(task.ExecuteSQLs)), 4)
	}
	task.Status = model.TaskStatusAudited

	err = st.UpdateTask(task, map[string]interface{}{
		"sql_type":  sqlType,
		"pass_rate": task.PassRate,
		"status":    task.Status,
	})
	if err != nil {
		entry.Errorf("update task to storage failed, error: %v", err)
		return err
	}

	if len(rollbackSqls) > 0 {
		err = st.UpdateRollbackSQLs(task, rollbackSqls)
		if err != nil {
			entry.Errorf("save rollback sql to storage failed, error: %v", err)
			return err
		}
	}
	return nil
}

func (s *Sqled) commit(task *model.Task) error {
	if task.SQLType == model.SQL_TYPE_DML {
		return s.commitDML(task)
	}

	if task.SQLType == model.SQL_TYPE_DDL {
		return s.commitDDL(task, false)
	}

	if task.SQLType == model.SQL_TYPE_PROCEDURE_FUNCTION {
		return s.commitDDL(task, true)
	}

	// if task is not inspected, parse task SQL type and commit it.
	entry := log.NewEntry().WithField("task_id", task.ID)
	i := inspector.NewInspector(entry, inspector.NewContext(nil), task, nil, nil)
	if err := i.ParseSqlType(); err != nil {
		return err
	}
	switch i.SqlType() {
	case model.SQL_TYPE_DML:
		return s.commitDML(task)
	case model.SQL_TYPE_DDL:
		return s.commitDDL(task, false)
	case model.SQL_TYPE_MULTI:
		return errors.SQL_STMT_CONFLICT_ERROR
	case model.SQL_TYPE_PROCEDURE_FUNCTION:
		return s.commitDDL(task, true)
	case model.SQL_TYPE_PROCEDURE_FUNCTION_MULTI:
		return errors.SQL_STMT_PROCEUDRE_FUNCTION_ERROR
	}
	return nil
}

func (s *Sqled) commitDDL(task *model.Task, isProcedureFunction bool) error {
	entry := log.NewEntry().WithField("task_id", task.ID)

	st := model.GetStorage()

	entry.Info("start commit")
	i := inspector.NewInspector(entry, inspector.NewContext(nil), task, nil, nil)
	for _, commitSql := range task.ExecuteSQLs {
		currentSql := commitSql
		err := i.Add(&currentSql.BaseSQL, func(sql *model.BaseSQL) error {
			err := st.UpdateExecuteSqlStatus(sql, model.SQLExecuteStatusDoing, "")
			if err != nil {
				i.Logger().Errorf("update commit sql status to storage failed, error: %v", err)
				return err
			}
			if isProcedureFunction {
				backupSqls, err := i.GetProcedureFunctionBackupSql(sql.Content)
				if err != nil {
					i.Logger().Errorf("get procedure function backup sql failed, error: %v", err)
					return err
				}
				if backupSqls != nil {
					for _, backupSql := range backupSqls {
						backupSqlModel := &model.BaseSQL{Content: backupSql}
						if err := i.CommitDDL(backupSqlModel); err != nil {
							i.Logger().Errorf("create procedure function backup failed, sql: %v, error: %v", backupSql, err)
						}
					}
				}
			}
			i.CommitDDL(sql)
			if sql.ExecResult != "ok" {
				err = st.Save(currentSql)
				if err != nil {
					i.Logger().Errorf("save commit sql to storage failed, error: %v", err)
				}
				return fmt.Errorf("exec ddl commit sql failed")
			}

			err = st.Save(currentSql)
			if err != nil {
				i.Logger().Errorf("save commit sql to storage failed, error: %v", err)
			}
			return err
		})
		if err != nil {
			entry.Error("add commit sql to task failed")
			return err
		}
	}

	if err := st.UpdateTaskStatusById(task.ID, model.TaskStatusExecuting); nil != err {
		return err
	}

	err := i.Do()
	if err != nil {
		entry.Error("commit sql failed")
	} else {
		entry.Info("commit sql finish")
	}

	taskStatus := model.TaskStatusExecuteSucceeded
	for _, sql := range task.ExecuteSQLs {
		if sql.ExecStatus == model.SQLExecuteStatusFailed {
			taskStatus = model.TaskStatusExecuteFailed
			break
		}
	}
	return st.UpdateTaskStatusById(task.ID, taskStatus)
}

func (s *Sqled) commitDML(task *model.Task) error {
	entry := log.NewEntry().WithField("task_id", task.ID)

	st := model.GetStorage()

	entry.Info("start commit")
	i := inspector.NewInspector(entry, inspector.NewContext(nil), task, nil, nil)
	sqls := []*model.BaseSQL{}

	err := st.UpdateExecuteSQLStatusByTaskId(task, model.SQLExecuteStatusDoing)
	if err != nil {
		i.Logger().Errorf("update commit sql status to storage failed, error: %v", err)
		return err
	}
	for _, executeSQL := range task.ExecuteSQLs {
		nodes, err := i.ParseSql(executeSQL.Content)
		if err != nil {
			return err
		}
		executeSQL.Stmts = nodes

		sqls = append(sqls, &executeSQL.BaseSQL)
	}

	if err := st.UpdateTaskStatusById(task.ID, model.TaskStatusExecuting); nil != err {
		return err
	}

	i.CommitDMLs(sqls)

	if err := st.UpdateExecuteSQLs(task, task.ExecuteSQLs); err != nil {
		i.Logger().Errorf("save commit sql to storage failed, error: %v", err)
		if err := st.UpdateTaskStatusById(task.ID, model.TaskStatusExecuteFailed); nil != err {
			log.Logger().Errorf("update task exec_status failed: %v", err)
		}
		return err
	}

	taskStatus := model.TaskStatusExecuteSucceeded
	for _, commitSql := range task.ExecuteSQLs {
		if commitSql.ExecStatus == model.SQLExecuteStatusFailed {
			taskStatus = model.TaskStatusExecuteFailed
			break
		}
	}
	return st.UpdateTaskStatusById(task.ID, taskStatus)
}

func (s *Sqled) rollback(task *model.Task) error {
	entry := log.NewEntry().WithField("task_id", task.ID)
	entry.Info("start rollback sql")

	st := model.GetStorage()
	i := inspector.NewInspector(entry, inspector.NewContext(nil), task, nil, nil)

	for _, rollbackSql := range task.RollbackSQLs {
		currentSql := rollbackSql
		if currentSql.Content == "" {
			continue
		}
		err := i.Add(&currentSql.BaseSQL, func(sql *model.BaseSQL) error {
			err := st.UpdateRollbackSqlStatus(sql, model.SQLExecuteStatusDoing, "")
			if err != nil {
				i.Logger().Errorf("update rollback sql status to storage failed, error: %v", err)
				return err
			}
			switch i.SqlType() {
			case model.SQL_TYPE_DDL:
				i.CommitDDL(sql)
			case model.SQL_TYPE_DML:
				i.CommitDMLs([]*model.BaseSQL{sql})
			case model.SQL_TYPE_MULTI:
				i.Logger().Error(errors.SQL_STMT_CONFLICT_ERROR)
				return errors.SQL_STMT_CONFLICT_ERROR
			case model.SQL_TYPE_PROCEDURE_FUNCTION:
				// need not rollback procedure and function
			case model.SQL_TYPE_PROCEDURE_FUNCTION_MULTI:
				i.Logger().Error((errors.SQL_STMT_PROCEUDRE_FUNCTION_ERROR))
				return errors.SQL_STMT_PROCEUDRE_FUNCTION_ERROR
			}
			err = st.Save(currentSql)
			if err != nil {
				i.Logger().Errorf("save commit sql to storage failed, error: %v", err)
			}
			return err
		})
		if err != nil {
			entry.Error("add rollback sql to task failed")
			return err
		}
	}
	err := i.Do()
	if err != nil {
		entry.Error("rollback sql failed")
	} else {
		entry.Info("rollback sql finish")
	}
	return err
}

func round(f float64, n int) float64 {
	p := math.Pow10(n)
	return math.Trunc(f*p+0.5) / p
}