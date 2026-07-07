package loom

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// -----------------------------------------------------------------------
// Task persistence (async provider jobs)
// -----------------------------------------------------------------------

// sqlInsertTask records an in-flight async provider job. It runs inside the step
// transaction (hence execer) and MUST be written before the results row, because
// loom_results.task_id references loom_tasks(id).
func sqlInsertTask(ctx context.Context, db execer, prefix string, th *TaskHandle, resultID uuid.UUID) error {
	now := time.Now()
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO %stasks (id, provider, handle, status, result_id, created_at, updated_at)
		VALUES ($1,$2,$3,'pending',$4,$5,$6)`, prefix),
		th.ID, th.Provider, th.Handle, resultID, now, now,
	)
	return err
}

func sqlListPendingTasks(ctx context.Context, db *sql.DB, prefix string) ([]pendingTask, error) {
	// Join through the linked result to the step so each pending task carries the
	// session and agent it belongs to. Every task is written in the same
	// transaction as its result and step, so the inner join never drops one.
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT t.id, t.provider, t.handle, t.attempts, s.session_id, s.agent_id
		FROM %stasks t
		JOIN %sresults r ON r.task_id = t.id
		JOIN %ssteps s ON s.result_id = r.id
		WHERE t.status='pending'`, prefix, prefix, prefix))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []pendingTask
	for rows.Next() {
		var t pendingTask
		if err := rows.Scan(&t.ID, &t.Provider, &t.Handle, &t.Attempts, &t.SessionID, &t.AgentID); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// sqlTaskStatus returns a task's status and, when ready, its resolved result.
func sqlTaskStatus(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID) (string, Result, error) {
	var (
		status, modal, rStatus string
		payload                []byte
	)
	err := db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT t.status, r.modality, r.status, r.payload
		FROM %stasks t
		JOIN %sresults r ON r.task_id = t.id
		WHERE t.id=$1`, prefix, prefix), id).Scan(&status, &modal, &rStatus, &payload)
	if err == sql.ErrNoRows {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, err
	}
	var result Result
	if status == "ready" {
		result = unmarshalResult(Modality(modal), ResultStatus(rStatus), payload)
	}
	return status, result, nil
}

func sqlIncrementTaskAttempts(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID, lastErr string) error {
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %stasks SET attempts=attempts+1, last_error=$2, updated_at=$3 WHERE id=$1`,
		prefix), id, lastErr, time.Now())
	return err
}

// sqlMarkTaskReady persists a resolved async Result. In one transaction it
// overwrites the linked results row (matched by task_id) with the resolved
// payload and status, then flips the task to ready. Previously the poller threw
// the resolved Result away, so the result stayed pending with an empty payload
// forever and the generated asset was unreachable.
func sqlMarkTaskReady(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID, result Result) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now()
	if result != nil {
		payload, err := marshalResultForStorage(result)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE %sresults SET status=$2, payload=$3, updated_at=$4 WHERE task_id=$1`, prefix),
			id, string(result.Status()), payload, now,
		)
		if err != nil {
			return err
		}
		// A resolved task must have a linked result row; refusing to flip the
		// task to ready when none was updated avoids silently losing the asset.
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return fmt.Errorf("loom: task %s has no linked result row to update", id)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %stasks SET status='ready', updated_at=$2 WHERE id=$1`, prefix),
		id, now,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// sqlMarkTaskFailed terminally fails an async job: it flips both the linked
// results row and the task to 'failed' so a stuck job stops being polled.
func sqlMarkTaskFailed(ctx context.Context, db *sql.DB, prefix string, id uuid.UUID, reason string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %sresults SET status='failed', updated_at=$2 WHERE task_id=$1`, prefix),
		id, now,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %stasks SET status='failed', last_error=$2, updated_at=$3 WHERE id=$1`, prefix),
		id, reason, now,
	); err != nil {
		return err
	}
	return tx.Commit()
}
