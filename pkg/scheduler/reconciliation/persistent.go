package reconciliation

import (
	"bytes"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/kyma-incubator/reconciler/pkg/cluster"
	"github.com/kyma-incubator/reconciler/pkg/db"
	"github.com/kyma-incubator/reconciler/pkg/model"
	"github.com/kyma-incubator/reconciler/pkg/repository"
	"github.com/pkg/errors"
)

type PersistentReconciliationRepository struct {
	*repository.Repository
}

func NewPersistedReconciliationRepository(conn db.Connection, debug bool) (Repository, error) {
	repo, err := repository.NewRepository(conn, debug)
	if err != nil {
		return nil, err
	}
	return &PersistentReconciliationRepository{repo}, nil
}

func (r *PersistentReconciliationRepository) CreateReconciliation(state *cluster.State, preComponents []string) (*model.ReconciliationEntity, error) {
	dbOps := func() (interface{}, error) {
		reconEntity := &model.ReconciliationEntity{
			Lock:                state.Cluster.RuntimeID,
			RuntimeID:           state.Cluster.RuntimeID,
			ClusterConfig:       state.Configuration.Version,
			ClusterConfigStatus: state.Status.ID,
			SchedulingID:        fmt.Sprintf("%s--%s", state.Cluster.RuntimeID, uuid.NewString()),
		}

		//find existing reconciliation for this cluster
		existingReconQ, err := db.NewQuery(r.Conn, reconEntity)
		if err != nil {
			return nil, err
		}
		existingRecon, err := existingReconQ.
			Select().
			Where(map[string]interface{}{
				"RuntimeID": state.Cluster.RuntimeID,
				"Finished":  false,
			}).
			GetOne()
		if err == nil {
			existingReconEntity := existingRecon.(*model.ReconciliationEntity)
			r.Logger.Infof("Found existing reconciliation for cluster '%s' (cluster-version:%d/config-version:%d) "+
				"which was created at '%s': cannot create another one",
				state.Cluster.RuntimeID, state.Cluster.Version, state.Configuration.Version, existingReconEntity.Created)
			return nil, newDuplicateClusterReconciliationError(existingReconEntity)
		}
		if err != sql.ErrNoRows {
			r.Logger.Errorf("Failed to check for existing reconciliations entities: %s", err)
			return nil, err
		}

		createReconQ, err := db.NewQuery(r.Conn, reconEntity)
		if err != nil {
			return nil, err
		}
		if err := createReconQ.Insert().Exec(); err != nil {
			r.Logger.Errorf("Failed to create new reconciliation entity for runtime '%s': %s",
				state.Cluster.RuntimeID, err)
			return nil, err
		}
		r.Logger.Debugf("New reconciliation for runtime '%s' with schedulingID '%s' created",
			state.Cluster.RuntimeID, reconEntity.SchedulingID)

		//get reconciliation sequence
		reconSeq, err := state.Configuration.GetReconciliationSequence(preComponents)
		if err != nil {
			r.Logger.Errorf("Failed to retrieve component models for runtime '%s': %s", state.Cluster.RuntimeID, err)
			return nil, err
		}

		//iterate over reconciliation sequence and create operations with proper priorities
		for idx, components := range reconSeq.Queue {
			priority := idx + 1
			for _, component := range components {
				createOpQ, err := db.NewQuery(r.Conn, &model.OperationEntity{
					Priority:      int64(priority),
					SchedulingID:  reconEntity.SchedulingID,
					CorrelationID: fmt.Sprintf("%s--%s", state.Cluster.RuntimeID, uuid.NewString()),
					RuntimeID:     reconEntity.RuntimeID,
					ClusterConfig: reconEntity.ClusterConfig,
					Component:     component.Component,
					State:         model.OperationStateNew,
					Updated:       time.Now().UTC(),
				})
				if err != nil {
					return nil, err
				}
				if err := createOpQ.Insert().Exec(); err != nil {
					r.Logger.Errorf("Failed to create operation for component '%s' with priority %d "+
						"(required for reconciliation of runtime '%s'): %s",
						component.Component, priority, state.Cluster.RuntimeID, err)
					return nil, err
				}
				r.Logger.Debugf("Created operation for component '%s' with priority %d "+
					"(required for reconciliation of runtime '%s')",
					component.Component, priority, state.Cluster.RuntimeID)
			}
			r.Logger.Debugf("Created %d operations with priority %d for reconciliation "+
				"of cluster '%s' with schedulingID '%s'",
				len(components), priority, state.Cluster.RuntimeID, reconEntity.SchedulingID)
		}

		return reconEntity, err
	}
	result, err := db.TransactionResult(r.Conn, dbOps, r.Logger)
	if err != nil {
		return nil, err
	}
	return result.(*model.ReconciliationEntity), nil
}

func (r *PersistentReconciliationRepository) RemoveReconciliation(schedulingID string) error {
	dbOps := func() error {
		whereCond := map[string]interface{}{
			"SchedulingID": schedulingID,
		}

		//delete operations
		qDelOps, err := db.NewQuery(r.Conn, &model.OperationEntity{})
		if err != nil {
			return err
		}
		delOpsCnt, err := qDelOps.Delete().
			Where(whereCond).
			Exec()
		if err != nil {
			return err
		}
		r.Logger.Debugf("Deleted %d operations which were assigned to reconciliation with schedulingID '%s'",
			delOpsCnt, schedulingID)

		//delete reconciliation
		qDelRecon, err := db.NewQuery(r.Conn, &model.ReconciliationEntity{})
		if err != nil {
			return err
		}
		delCnt, err := qDelRecon.Delete().
			Where(whereCond).
			Exec()
		r.Logger.Debugf("Deleted %d reconciliation with schedulingID '%s'", delCnt, schedulingID)
		return err
	}
	return db.Transaction(r.Conn, dbOps, r.Logger)
}

func (r *PersistentReconciliationRepository) GetReconciliation(schedulingID string) (*model.ReconciliationEntity, error) {
	q, err := db.NewQuery(r.Conn, &model.ReconciliationEntity{})
	if err != nil {
		return nil, err
	}
	whereCond := map[string]interface{}{
		"SchedulingID": schedulingID,
	}
	reconEntity, err := q.Select().
		Where(whereCond).
		GetOne()
	if err != nil {
		return nil, r.NewNotFoundError(err, reconEntity, whereCond)
	}
	return reconEntity.(*model.ReconciliationEntity), nil
}

func (r *PersistentReconciliationRepository) FinishReconciliation(schedulingID string, status *model.ClusterStatusEntity) error {
	dbOps := func() error {
		//get running reconciliation
		reconEntity, err := r.GetReconciliation(schedulingID)
		if err != nil {
			return err
		}

		//update reconciliation and remove lock
		reconEntity.Lock = fmt.Sprintf("unlock-%s", reconEntity.SchedulingID)
		reconEntity.Finished = true
		reconEntity.ClusterConfigStatus = status.ID
		reconEntity.Updated = time.Now().UTC()
		updReconQ, err := db.NewQuery(r.Conn, reconEntity)
		if err != nil {
			return err
		}
		cnt, err := updReconQ.Update().
			Where(
				map[string]interface{}{
					"SchedulingID": schedulingID,
					"Finished":     false,
				}).
			ExecCount()
		if err != nil {
			return err
		}
		if cnt == 0 {
			return fmt.Errorf("failed to update reconciliation with schedulingID '%s' "+
				"(maybe updated by parallel running process)", schedulingID)
		}

		return nil
	}
	return db.Transaction(r.Conn, dbOps, r.Logger)
}

func (r *PersistentReconciliationRepository) GetReconciliations(filter Filter) ([]*model.ReconciliationEntity, error) {
	q, err := db.NewQuery(r.Conn, &model.ReconciliationEntity{})
	if err != nil {
		return nil, err
	}

	//fire query
	selectQ := q.Select()
	if filter != nil {
		if err := filter.FilterByQuery(selectQ); err != nil {
			return nil, errors.Wrap(err, "failed to apply sql filter")
		}
	}
	recons, err := selectQ.GetMany()
	if err != nil {
		return nil, err
	}

	var result []*model.ReconciliationEntity
	for _, recon := range recons {
		result = append(result, recon.(*model.ReconciliationEntity))
	}
	return result, nil
}

func (r *PersistentReconciliationRepository) GetOperations(schedulingID string, states ...model.OperationState) ([]*model.OperationEntity, error) {
	q, err := db.NewQuery(r.Conn, &model.OperationEntity{})
	if err != nil {
		return nil, err
	}

	selectQ := q.Select().
		Where(map[string]interface{}{
			"SchedulingID": schedulingID,
		})

	if len(states) > 0 {
		var args []interface{}
		var buffer bytes.Buffer

		//add state to args array and generate placeholder string for SQL stmt
		for idx, state := range states {
			args = append(args, state)
			if buffer.Len() > 0 {
				buffer.WriteRune(',')
			}
			buffer.WriteString(fmt.Sprintf("$%d", idx+2))
		}

		selectQ.WhereIn("State", buffer.String(), args...)
	}

	ops, err := selectQ.GetMany()
	if err != nil {
		return nil, err
	}

	var result []*model.OperationEntity
	for _, op := range ops {
		result = append(result, op.(*model.OperationEntity))
	}
	return result, nil
}

func (r *PersistentReconciliationRepository) GetOperation(schedulingID, correlationID string) (*model.OperationEntity, error) {
	q, err := db.NewQuery(r.Conn, &model.OperationEntity{})
	if err != nil {
		return nil, err
	}
	whereCond := map[string]interface{}{
		"CorrelationID": correlationID,
		"SchedulingID":  schedulingID,
	}
	opEntity, err := q.Select().
		Where(whereCond).
		GetOne()
	if err != nil {
		return nil, r.NewNotFoundError(err, opEntity, whereCond)
	}
	return opEntity.(*model.OperationEntity), nil
}

func (r *PersistentReconciliationRepository) GetProcessableOperations(maxParallelOpsPerRecon int) ([]*model.OperationEntity, error) {
	opEntities, err := r.GetReconcilingOperations()
	if err != nil {
		return nil, err
	}
	return findProcessableOperations(opEntities, maxParallelOpsPerRecon), nil
}

func (r *PersistentReconciliationRepository) GetReconcilingOperations() ([]*model.OperationEntity, error) {
	//retrieve all non-finished operations
	reconEntity := &model.ReconciliationEntity{}
	colHdr, err := db.NewColumnHandler(reconEntity, r.Conn)
	if err != nil {
		return nil, err
	}
	schedulingIDCol, err := colHdr.ColumnName("SchedulingID")
	if err != nil {
		return nil, err
	}
	FinishedCol, err := colHdr.ColumnName("Finished")
	if err != nil {
		return nil, err
	}
	q, err := db.NewQuery(r.Conn, &model.OperationEntity{})
	if err != nil {
		return nil, err
	}
	ops, err := q.Select().
		WhereIn(
			"SchedulingID",
			//consider only operations which are part of a running reconciliations
			fmt.Sprintf("SELECT %s FROM %s WHERE %s=$1", schedulingIDCol, reconEntity.Table(), FinishedCol), false).
		GetMany()
	if err != nil {
		return nil, err
	}

	var opEntities []*model.OperationEntity
	for _, op := range ops {
		opEntities = append(opEntities, op.(*model.OperationEntity))
	}
	return opEntities, nil
}

func (r *PersistentReconciliationRepository) UpdateOperationState(schedulingID, correlationID string, state model.OperationState, reasons ...string) error {
	dbOps := func() error {
		op, err := r.GetOperation(schedulingID, correlationID)
		if err != nil {
			if repository.IsNotFoundError(err) {
				r.Logger.Warnf("operation not found (schedulingID:%s/correlationID:%s)", schedulingID, correlationID)
			}
			return err
		}

		if op.State == model.OperationStateDone || op.State == model.OperationStateError {
			return fmt.Errorf("cannot update state of operation '%s' to new state '%s' "+
				"because operation is already in final state '%s'", op.Component, state, op.State)
		}

		//update operation-entity
		opStateOld := op.State //required in where-condition later on
		op.State = state
		reason, err := concatStateReasons(state, reasons)
		if err != nil {
			return err
		}
		op.Reason = reason
		op.Updated = time.Now().UTC()

		//prepare update query
		q, err := db.NewQuery(r.Conn, op)
		if err != nil {
			return err
		}
		whereCond := map[string]interface{}{
			"CorrelationID": correlationID,
			"SchedulingID":  schedulingID,
			"State":         opStateOld, //ensure update will affect only operations which were not updated in between
		}
		cnt, err := q.Update().
			Where(whereCond).
			ExecCount()

		if cnt == 0 {
			return fmt.Errorf("update of operation '%s' was not successful: "+
				"seems the operation does no longer match the where-conditions (no row was updated)",
				op)
		}

		return err
	}
	return db.Transaction(r.Conn, dbOps, r.Logger)
}
