package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// -----------------------------------------------------------------------
// Step persistence
// -----------------------------------------------------------------------

// execer is satisfied by both *sql.DB and *sql.Tx, letting the row-insert
// helpers run either standalone or inside a transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// sqlInsertSnapshot records a per-step state checkpoint so a later Fork can
// reconstruct full session state as of a given step instead of inheriting the
// parent's latest. The whole State (modality, opaque snapshot, vars, available
// actions) is serialized into the snapshot column; the vars column is also
// populated for external introspection.
func sqlInsertSnapshot(ctx context.Context, db execer, prefix string, id, sessionID uuid.UUID, stepIndex int, state State, createdAt time.Time) error {
	stateJSON, _ := json.Marshal(state)
	varsJSON, _ := json.Marshal(state.Vars)
	if len(varsJSON) == 0 || string(varsJSON) == "null" {
		varsJSON = []byte("{}")
	}
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %sstate_snapshots (id, session_id, step_index, snapshot, vars, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)`, prefix),
		id, sessionID, stepIndex, stateJSON, varsJSON, createdAt)
	return err
}

// sqlQuerySnapshotAt returns the full checkpointed State at or before stepIndex
// for a session. found is false when no checkpoint exists that early.
func sqlQuerySnapshotAt(ctx context.Context, db *sql.DB, prefix string, sessionID uuid.UUID, stepIndex int) (state State, found bool, err error) {
	var stateJSON []byte
	row := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT snapshot FROM %sstate_snapshots
		WHERE session_id = $1 AND step_index <= $2
		ORDER BY step_index DESC, created_at DESC
		LIMIT 1`, prefix), sessionID, stepIndex)
	switch scanErr := row.Scan(&stateJSON); scanErr {
	case nil:
	case sql.ErrNoRows:
		return State{}, false, nil
	default:
		return State{}, false, scanErr
	}
	if len(stateJSON) > 0 {
		_ = json.Unmarshal(stateJSON, &state)
	}
	return state, true, nil
}

// sqlInsertStep persists the step (with its result and action) and, when
// checkpoint is non-nil, the post-step state snapshot — all in one transaction
// so a step never commits without its checkpoint (which would let a later Fork
// silently rewind to the wrong state).
func sqlInsertStep(ctx context.Context, db *sql.DB, prefix string, step *Step, checkpoint *State, cost *CostRecord) error {
	reqJSON, _ := json.Marshal(step.Request)
	diagJSON, _ := json.Marshal(step.Diagnostics)

	// All writes (action, result, step, snapshot, cost) commit atomically so a
	// failure on a later INSERT cannot orphan earlier rows, split a step from
	// its checkpoint, or leave a durable step with no recorded cost.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var actionID *string
	if step.Action != nil {
		s := step.Action.ID.String()
		actionID = &s
		if err := sqlInsertAction(ctx, tx, prefix, step.SessionID, step.Index, step.Action); err != nil {
			return fmt.Errorf("persist action: %w", err)
		}
	}
	// An async result carries a TaskHandle. Record the task row first so the
	// background poller can later resolve it, and so the results.task_id foreign
	// key is satisfied (loom_results.task_id references loom_tasks.id).
	if th := step.Result.TaskHandle(); th != nil {
		if err := sqlInsertTask(ctx, tx, prefix, th, step.Result.ResultID()); err != nil {
			return fmt.Errorf("persist task: %w", err)
		}
	}
	if err := sqlInsertResult(ctx, tx, prefix, step); err != nil {
		return fmt.Errorf("persist result: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %ssteps
			(id, session_id, step_index, agent_id, request, result_id,
			 action_id, diagnostics, duration_ms, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, prefix),
		step.ID, step.SessionID, step.Index, step.AgentID,
		reqJSON, step.Result.ResultID(),
		actionID, diagJSON, step.DurationMs, step.CreatedAt,
	); err != nil {
		return err
	}
	if checkpoint != nil {
		if err := sqlInsertSnapshot(ctx, tx, prefix, uuid.New(), step.SessionID, step.Index, *checkpoint, step.CreatedAt); err != nil {
			return fmt.Errorf("persist snapshot: %w", err)
		}
	}
	// A contained cost failure must NOT abort the step, so it is carried past
	// the commit rather than returned here — returning early would trip the
	// deferred Rollback and discard the very step we are trying to preserve.
	var costErr error
	if cost != nil {
		if err := insertCostSavepoint(ctx, tx, prefix, *cost); err != nil {
			var cre *costRecordError
			if !errors.As(err, &cre) {
				return err // savepoint machinery failed; the tx is unusable
			}
			costErr = err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return costErr
}

// insertCostSavepoint writes the step's cost record inside the step's own
// transaction, so a durable step always has durable cost — this used to be a
// detached `go` with context.Background(), which meant a redeploy between the
// step returning and the insert landing silently dropped that step's cost and
// every aggregate built on it under-reported.
//
// The insert is wrapped in a SAVEPOINT because the generation has ALREADY been
// billed by the provider by this point. Discarding a completed, paid-for step
// because a bookkeeping row failed would be a strictly worse outcome than
// losing the bookkeeping row, so an independent cost failure rolls back only
// this insert and is logged by the caller; the step still commits.
//
// The ROLLBACK TO is not optional on Postgres: an error inside a transaction
// poisons it (25P02) until the savepoint is unwound, so without this a failed
// cost insert would take the whole step down anyway. Both dialects support the
// SAVEPOINT / ROLLBACK TO / RELEASE trio.
func insertCostSavepoint(ctx context.Context, tx *sql.Tx, prefix string, rec CostRecord) error {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT loom_cost`); err != nil {
		return fmt.Errorf("cost savepoint: %w", err)
	}
	if insErr := sqlInsertCostRecord(ctx, tx, prefix, rec); insErr != nil {
		if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT loom_cost`); err != nil {
			// The savepoint could not be unwound, so the transaction is now
			// unusable — surfacing this is better than committing a step whose
			// tx is in an undefined state.
			return fmt.Errorf("cost rollback after %v: %w", insErr, err)
		}
		return &costRecordError{err: insErr}
	}
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT loom_cost`); err != nil {
		return fmt.Errorf("cost release savepoint: %w", err)
	}
	return nil
}

// costRecordError marks a cost-write failure that was contained by the
// savepoint. The step is still committable; the caller logs and continues.
type costRecordError struct{ err error }

func (e *costRecordError) Error() string { return "loom: record cost: " + e.err.Error() }
func (e *costRecordError) Unwrap() error { return e.err }

func sqlInsertResult(ctx context.Context, db execer, prefix string, step *Step) error {
	payload, _ := marshalResultForStorage(step.Result)
	var taskID *string
	if th := step.Result.TaskHandle(); th != nil {
		s := th.ID.String()
		taskID = &s
	}
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %sresults (id, modality, status, payload, task_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, prefix),
		step.Result.ResultID(), string(step.Result.Modality()),
		string(step.Result.Status()), payload, taskID,
		time.Now(), time.Now(),
	)
	return err
}

func sqlInsertAction(ctx context.Context, db execer, prefix string, sessionID uuid.UUID, stepIndex int, action *Action) error {
	payloadJSON, _ := json.Marshal(action.Payload)
	metaJSON, _ := json.Marshal(action.Metadata)
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %sactions (id, session_id, step_index, kind, payload, metadata, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, prefix),
		action.ID, sessionID, stepIndex, string(action.Kind),
		payloadJSON, metaJSON, time.Now(),
	)
	return err
}

func sqlQuerySteps(ctx context.Context, db *sql.DB, prefix string, sessionID uuid.UUID) ([]Step, error) {
	return sqlQueryStepsPage(ctx, db, prefix, sessionID, 0, 0)
}

// sqlQueryStepsPage reads a session's steps ordered by index. limit <= 0 means
// no limit. Each row carries the fully rendered request and the result payload,
// so an unbounded read grows with session length — hence the paged variant.
func sqlQueryStepsPage(ctx context.Context, db *sql.DB, prefix string, sessionID uuid.UUID, limit, offset int) ([]Step, error) {
	q := fmt.Sprintf(`
		SELECT s.id, s.session_id, s.step_index, s.agent_id,
		       s.request, s.diagnostics, s.duration_ms, s.created_at,
		       r.modality, r.status, r.payload
		FROM %ssteps s
		JOIN %sresults r ON r.id = s.result_id
		WHERE s.session_id=$1
		ORDER BY s.step_index`, prefix, prefix)
	args := []any{sessionID}
	if limit > 0 {
		// OFFSET without LIMIT is not portable, so it is only applied alongside one.
		q += ` LIMIT $2 OFFSET $3`
		args = append(args, limit, max(offset, 0))
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var steps []Step
	for rows.Next() {
		var (
			step              Step
			reqJSON, diagJSON []byte
			modal, status     string
			payload           []byte
		)
		if err := rows.Scan(
			&step.ID, &step.SessionID, &step.Index, &step.AgentID,
			&reqJSON, &diagJSON, &step.DurationMs, &step.CreatedAt,
			&modal, &status, &payload,
		); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(reqJSON, &step.Request)
		_ = json.Unmarshal(diagJSON, &step.Diagnostics)
		step.Result = unmarshalResult(Modality(modal), ResultStatus(status), payload)
		steps = append(steps, step)
	}
	return steps, rows.Err()
}
