package registry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (r AcceptRequest) mutation() MutationRequest {
	return MutationRequest{
		PrincipalID: r.PrincipalID, CredentialID: r.CredentialID, PrincipalScope: r.PrincipalScope,
		Epoch: r.Epoch, Key: r.Key, Scope: r.Scope, Request: r.Request,
		ExpectedRevision: r.ExpectedRevision, ExpectedGeneration: r.ExpectedGeneration,
	}
}

func validateMutation(r MutationRequest) error {
	if !validateLabel(r.PrincipalID, 256) || !validateLabel(r.Epoch, 256) ||
		!validateLabel(r.Scope, 1024) || !validateLabel(r.Key, 256) || r.ExpectedRevision < 0 || r.ExpectedGeneration < 0 {
		return fmt.Errorf("%w: principal, epoch, operation scope, and idempotency key are required", ErrInvalid)
	}
	method, path, ok := strings.Cut(r.Scope, " ")
	if !ok || method == "" || method != strings.ToUpper(method) || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, " ?#") {
		return fmt.Errorf("%w: operation scope must be method and canonical path", ErrInvalid)
	}
	return nil
}

func mutationFingerprint(ctx context.Context, tx *sql.Tx, r MutationRequest, target any) (string, error) {
	control, err := readControl(ctx, tx)
	if err != nil {
		return "", err
	}
	if control.ControlEpoch != r.Epoch {
		return "", ErrControlEpoch
	}

	var disabled bool
	err = tx.QueryRowContext(ctx, "SELECT disabled FROM principals WHERE id = ?", r.PrincipalID).Scan(&disabled)
	if errors.Is(err, sql.ErrNoRows) || err == nil && disabled {
		return "", ErrUnauthorized
	}
	if err != nil {
		return "", err
	}
	actorScope := r.PrincipalScope
	if r.CredentialID != "" {
		var scope string
		err := tx.QueryRowContext(ctx, `SELECT scope_json FROM credentials
			WHERE id = ? AND principal_id = ? AND kind IN ('api_token', 'web_session')
			AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`,
			r.CredentialID, r.PrincipalID, time.Now().UnixMilli()).Scan(&scope)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrUnauthorized
		}
		if err != nil {
			return "", err
		}
		actorScope = json.RawMessage(scope)
	}
	scope, err := canonicalJSON(actorScope)
	if err != nil {
		return "", err
	}
	request, err := canonicalJSON(r.Request)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Actor      json.RawMessage
		Business   json.RawMessage
		Path       string
		Revision   int64
		Generation int64
		Target     any
	}{scope, request, r.Scope, r.ExpectedRevision, r.ExpectedGeneration, target})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

type idempotencyRecord struct {
	Hash        string
	OperationID string
	Response    json.RawMessage
}

func readIdempotency(ctx context.Context, tx *sql.Tx, r MutationRequest) (idempotencyRecord, bool, error) {
	var record idempotencyRecord
	var response string
	err := tx.QueryRowContext(ctx, `SELECT request_hash, COALESCE(operation_id, ''), COALESCE(response_json, '')
		FROM idempotency WHERE control_epoch = ? AND principal_id = ? AND scope = ? AND key = ?`,
		r.Epoch, r.PrincipalID, r.Scope, r.Key).Scan(&record.Hash, &record.OperationID, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return record, false, nil
	}
	record.Response = json.RawMessage(response)
	return record, err == nil, err
}

func checkWritable(ctx context.Context, tx *sql.Tx) error {
	control, err := readControl(ctx, tx)
	if err != nil {
		return err
	}
	if control.Maintenance {
		return ErrMaintenance
	}
	return nil
}

// Mutate atomically applies a synchronous change and records its retry response.
// When RetryResponse is supplied, Response is returned only on the first call.
func (s *Store) Mutate(ctx context.Context, r MutationRequest, apply func(context.Context, *sql.Tx) (MutationResult, error)) (json.RawMessage, bool, error) {
	if err := validateMutation(r); err != nil {
		return nil, false, err
	}
	if apply == nil {
		return nil, false, fmt.Errorf("%w: missing mutation handler", ErrInvalid)
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	fingerprint, err := mutationFingerprint(ctx, tx, r, nil)
	if err != nil {
		return nil, false, err
	}
	existing, found, err := readIdempotency(ctx, tx, r)
	if err != nil {
		return nil, false, err
	}
	if found {
		if existing.Hash != fingerprint || existing.OperationID != "" {
			return nil, false, ErrIdempotency
		}
		return existing.Response, true, nil
	}
	if err := checkWritable(ctx, tx); err != nil {
		return nil, false, err
	}
	result, err := apply(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	response, err := ensureJSON(result.Response, "null")
	if err != nil {
		return nil, false, err
	}
	retry, err := ensureJSON(result.RetryResponse, response)
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency
		(control_epoch, principal_id, scope, key, request_hash, response_json) VALUES (?, ?, ?, ?, ?, ?)`,
		r.Epoch, r.PrincipalID, r.Scope, r.Key, fingerprint, retry); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return json.RawMessage(response), false, nil
}

// CompleteMutation replaces the reservation receipt for an externally
// dispatched change. The caller may only complete the exact reservation.
func (s *Store) CompleteMutation(ctx context.Context, r MutationRequest, reservation, receipt json.RawMessage) error {
	if !json.Valid(receipt) {
		return ErrInvalid
	}
	result, err := s.writer.ExecContext(ctx, `UPDATE idempotency SET response_json=?
		WHERE control_epoch=? AND principal_id=? AND scope=? AND key=? AND response_json=? AND operation_id IS NULL`,
		string(receipt), r.Epoch, r.PrincipalID, r.Scope, r.Key, string(reservation))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrIdempotency
	}
	return nil
}
