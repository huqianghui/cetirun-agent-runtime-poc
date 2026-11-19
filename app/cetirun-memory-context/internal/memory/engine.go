// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package memory

import (
	"context"
	"errors"
	"slices"
	"syscall"
)

func (s *Store) Execute(ctx context.Context, command string, r Request) (Response, int) {
	response := Response{Reason: "OK"}
	if r.ContractVersion != ContractVersion {
		return failed(response, problem(6, "SchemaUnsupported"))
	}
	if command == "status" {
		var input struct {
			LookupRequestID string `json:"lookupRequestId,omitempty"`
		}
		if err := Decode(r.Payload, &input); err != nil {
			return failed(response, err)
		}
		v, err := s.openRead(ctx)
		if errors.Is(err, syscall.ENOENT) {
			response.OK = true
			response.Data = map[string]any{"initialized": false, "contractVersion": ContractVersion, "helperABI": HelperABI, "mode": "enabled", "learning": defaultLearning()}
			return response, 0
		}
		if err != nil {
			return failed(response, err)
		}
		defer v.fs.close()
		response.Generation = v.m.Generation
		data := map[string]any{"initialized": true, "mode": "enabled", "learning": defaultLearning(), "contractVersion": ContractVersion, "helperABI": HelperABI, "runtimeAssurance": "cooperative-files", "modelEvaluation": "native-adapter-required", "adapterVersion": AdapterVersion}
		pending, err := v.pendingPurges()
		if err != nil {
			return failed(response, err)
		}
		data["pendingPurges"] = pending
		data["purgePending"] = len(pending) > 0
		if input.LookupRequestID != "" {
			receipt, ok := v.m.Requests[requestKey(input.LookupRequestID)]
			if ok {
				if receipt.Result.PurgePending {
					receipt.Result.PurgePending = false
					for key, plan := range v.m.Purges {
						if plan.Generation == receipt.Generation-1 && slices.Contains(pending, key) {
							receipt.Result.PurgePending = true
							break
						}
					}
				}
				data["requestState"] = "committed"
				data["result"] = receipt.Result
				for _, ref := range receipt.Result.Refs {
					if v.m.Tombstones[ref.ID] {
						data["requestState"] = "forgotten"
						delete(data, "result")
						break
					}
				}
			} else {
				data["requestState"] = "unknown"
			}
		}
		response.OK = true
		response.Data = data
		return response, 0
	}
	if command == "init" {
		var input struct{}
		if err := Decode(r.Payload, &input); err != nil {
			return failed(response, err)
		}
		result, generation, err := s.init(ctx, r)
		response.Generation = generation
		response.Data = result
		if err != nil {
			return failed(response, err)
		}
		response.OK = true
		return response, 0
	}
	maintenance := command == "forget-plan" || command == "forget-apply" || command == "import-plan" || command == "import-apply"
	readOnly := command == "query" || command == "prepare" || command == "trace" || command == "lint" || command == "forget-plan" || command == "import-plan" || command == "select" && (r.RequestContext.NoCapture || r.RequestContext.Delegation != nil && r.RequestContext.Delegation.NoCapture)
	if err := validateContext(r, !readOnly, maintenance); err != nil {
		return failed(response, err)
	}
	if command == "repair-index" {
		generation, partial, err := s.repair(ctx, r)
		response.Generation = generation
		response.Partial = partial
		if err != nil {
			return failed(response, err)
		}
		response.OK = true
		return response, 0
	}
	if readOnly {
		if command == "import-plan" {
			data, err := s.importPlan(r)
			if err != nil {
				return failed(response, err)
			}
			response.OK = true
			response.Data = data
			return response, 0
		}
		v, err := s.openRead(ctx)
		if err != nil {
			return failed(response, err)
		}
		defer v.fs.close()
		response.Generation = v.m.Generation
		switch command {
		case "query":
			var result QueryResult
			result, err = s.query(ctx, v, r)
			response.Data = result
			response.Partial = result.Partial
		case "trace":
			var result traceResult
			result, err = s.trace(v, r)
			response.Data = result
			response.Partial = &result.Partial
		case "forget-plan":
			response.Data, err = s.forgetPlan(v, r)
		case "select":
			response.Data, err = s.selectMethod(v, r, false)
		case "lint":
			err = v.checkViews()
			if err == nil {
				for _, ref := range v.m.Objects {
					if !v.allowed(ref, r) {
						continue
					}
					if _, readErr := dependencies(v, ref); readErr != nil {
						if v.damagedObject(ref, readErr) {
							continue
						}
						err = readErr
						break
					}
				}
			}
			response.Partial = v.damageReport()
		case "prepare":
			var input struct{}
			if err = Decode(r.Payload, &input); err != nil {
				break
			}
			pending := []Observation{}
			for _, id := range v.m.Pending {
				if len(pending) >= 8 {
					break
				}
				ref, ok := v.m.Objects[id]
				if !ok {
					err = problem(6, "IntegrityMismatch")
					break
				}
				if !v.allowed(ref, r) {
					continue
				}
				var item Observation
				if err = v.read(ref, &item); err != nil {
					break
				}
				pending = append(pending, item)
			}
			states := v.m.ProcedureStates
			if d := r.RequestContext.Delegation; d != nil {
				states = map[string]methodState{}
				for _, ref := range d.AllowedRefs {
					if state, ok := v.m.ProcedureStates[ref.Key()]; ok {
						states[ref.Key()] = state
					}
				}
			}
			response.Data = map[string]any{"observations": pending, "methodStates": states}
			if len(v.m.Pending) > len(pending) {
				response.Partial = &Partial{LimitHit: true, Unscanned: len(v.m.Pending) - len(pending)}
			}
		}
		if err != nil {
			return failed(response, err)
		}
		response.OK = true
		return response, 0
	}
	result, generation, err := s.write(ctx, r, command, maintenance, func(v *snapshot) (writeResult, error) {
		switch command {
		case "capture":
			return s.capture(v, r)
		case "revise":
			return s.revise(v, r)
		case "commit":
			return s.commitBatch(v, r)
		case "propose":
			return s.propose(v, r)
		case "check":
			return s.check(v, r)
		case "advance":
			return s.advance(v, r)
		case "select":
			return s.selectMethod(v, r, true)
		case "retire":
			return s.retire(v, r)
		case "record-source-check":
			return s.recordSourceCheck(v, r)
		case "record-use":
			return s.recordUse(v, r)
		case "forget-apply":
			return s.forgetApply(v, r)
		case "import-apply":
			return s.importApply(v, r)
		default:
			return writeResult{}, problem(2, "InvalidCommand")
		}
	})
	response.Generation = generation
	response.Data = result
	if err != nil {
		return failed(response, err)
	}
	if command == "forget-apply" {
		var input forgetApplyInput
		if Decode(r.Payload, &input) == nil {
			result.PurgePending = !s.purge(ctx, input.Plan.Digest)
			response.Data = result
		}
	}
	response.OK = true
	return response, 0
}
func failed(response Response, err error) (Response, int) {
	var typed *Error
	if !errors.As(err, &typed) {
		typed = &Error{8, "StorageError"}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			typed = &Error{5, "Cancelled"}
		}
	}
	response.OK = false
	response.Reason = typed.Reason
	response.Data = nil
	response.Retryable = typed.Reason == "LockBusy"
	return response, typed.Code
}
