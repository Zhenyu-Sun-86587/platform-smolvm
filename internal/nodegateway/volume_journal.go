package nodegateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
)

type volumeEntry struct {
	Scope   nodeproto.Route
	Digest  string
	Receipt *rt.VolumeReceipt
	Refusal string
}

func lookupVolume(ctx context.Context, q queryer, id string) (entry volumeEntry, err error) {
	var scope string
	var receipt, refusal sql.NullString
	err = q.QueryRowContext(ctx, "SELECT scope,digest,receipt,refusal FROM volume_commands WHERE operation_id=?", id).
		Scan(&scope, &entry.Digest, &receipt, &refusal)
	if err != nil {
		return entry, err
	}
	if err = json.Unmarshal([]byte(scope), &entry.Scope); err != nil {
		return entry, err
	}
	if receipt.Valid {
		entry.Receipt = new(rt.VolumeReceipt)
		err = json.Unmarshal([]byte(receipt.String), entry.Receipt)
	}
	entry.Refusal = refusal.String
	return entry, err
}

func (j *Journal) beginVolume(ctx context.Context, scope nodeproto.Route, in nodeproto.VolumeMutation, digest string) (entry volumeEntry, send bool, err error) {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return entry, false, err
	}
	defer tx.Rollback()
	var native bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM commands WHERE operation_id=?)", in.OperationID).Scan(&native); err != nil {
		return entry, false, err
	}
	if native {
		return entry, false, &nodeproto.Failure{Code: nodeproto.CodeConflict}
	}
	current, err := lookupVolume(ctx, tx, in.OperationID)
	if err == nil {
		if current.Scope != scope || current.Digest != digest {
			return entry, false, &nodeproto.Failure{Code: nodeproto.CodeConflict}
		}
		return current, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return entry, false, err
	}
	action := in.Command.Action
	if err = gateAllows(ctx, tx, action == "create", action == "attach" || action == "grow"); err != nil {
		return entry, false, err
	}
	raw, err := json.Marshal(scope)
	if err != nil {
		return entry, false, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO volume_commands(operation_id,scope,digest) VALUES(?,?,?)", in.OperationID, string(raw), digest); err != nil {
		return entry, false, err
	}
	return volumeEntry{Scope: scope, Digest: digest}, true, tx.Commit()
}

func (j *Journal) finishVolume(ctx context.Context, id string, receipt *rt.VolumeReceipt, refusal string) error {
	var receiptValue, refusalValue sql.NullString
	if receipt != nil {
		raw, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		receiptValue = sql.NullString{String: string(raw), Valid: true}
	}
	if refusal != "" {
		refusalValue = sql.NullString{String: refusal, Valid: true}
	}
	_, err := j.db.ExecContext(ctx, "UPDATE volume_commands SET receipt=?,refusal=? WHERE operation_id=? AND receipt IS NULL AND refusal IS NULL",
		receiptValue, refusalValue, id)
	return err
}
