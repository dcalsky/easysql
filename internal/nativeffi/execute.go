// Package nativeffi implements the versioned, JSON-based contract exposed by
// the easysql native library. It deliberately contains no cgo so the protocol
// and every operation can be exercised by ordinary Go tests.
package nativeffi

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dcalsky/easysql"
)

const (
	ABIVersion uint32 = 1

	StatusSuccess         int32 = 0
	StatusInvalidArgument int32 = 1
	StatusParseError      int32 = 2
	StatusUnsupported     int32 = 3
	StatusInternalError   int32 = 4
	StatusPanic           int32 = 99
)

// LibraryVersion is replaced by the release build with -X. Keeping a usable
// default makes local go test and ad-hoc builds deterministic.
var LibraryVersion = "dev"

type request struct {
	ABIVersion uint32          `json:"abiVersion"`
	Operation  string          `json:"operation"`
	Args       json.RawMessage `json:"args"`
}

type response struct {
	ABIVersion uint32 `json:"abiVersion"`
	Status     int32  `json:"status"`
	Data       any    `json:"data,omitempty"`
	Error      string `json:"error,omitempty"`
}

type applyRowFilterArgs struct {
	SQL          string   `json:"sql"`
	WhereClause  string   `json:"whereClause"`
	Dialect      string   `json:"dialect,omitempty"`
	TableNames   []string `json:"tableNames,omitempty"`
	TableRegexps []string `json:"tableRegexps,omitempty"`
	DefaultDB    string   `json:"defaultDB,omitempty"`
}

type bindCTEsArgs struct {
	ConsumerSQL string       `json:"consumerSQL"`
	Bindings    []cteBinding `json:"bindings"`
	Dialect     string       `json:"dialect,omitempty"`
}

type cteBinding struct {
	Name  string `json:"name"`
	Query string `json:"query"`
}

type lineageArgs struct {
	SQL       string              `json:"sql"`
	Dialect   string              `json:"dialect,omitempty"`
	Metadata  map[string][]string `json:"metadata,omitempty"`
	Producer  string              `json:"producer,omitempty"`
	Namespace string              `json:"namespace,omitempty"`
}

type rewriteTableReferencesArgs struct {
	SQL                string         `json:"sql"`
	Specs              []tableRewrite `json:"specs"`
	Dialect            string         `json:"dialect,omitempty"`
	StripMatchCatalogs []string       `json:"stripMatchCatalogs,omitempty"`
}

type tableRewrite struct {
	MatchKey string        `json:"matchKey"`
	Inline   *tableRef     `json:"inline,omitempty"`
	Union    *unionRewrite `json:"union,omitempty"`
}

type tableRef struct {
	Catalog string `json:"catalog,omitempty"`
	Schema  string `json:"schema,omitempty"`
	Table   string `json:"table"`
}

type unionRewrite struct {
	TableAlias string     `json:"tableAlias,omitempty"`
	Columns    []string   `json:"columns"`
	Branches   []tableRef `json:"branches"`
}

// Execute validates and executes one ABI request. It always returns a JSON
// response and converts panics into a generic internal error so no panic or
// implementation detail crosses the native boundary.
func Execute(input []byte) (output []byte) {
	defer func() {
		if recover() != nil {
			output = marshal(response{
				ABIVersion: ABIVersion,
				Status:     StatusPanic,
				Error:      "easysql: internal panic",
			})
		}
	}()

	var req request
	if len(input) == 0 {
		return failure(StatusInvalidArgument, "easysql: request must not be empty")
	}
	if err := json.Unmarshal(input, &req); err != nil {
		return failure(StatusInvalidArgument, fmt.Sprintf("easysql: invalid request JSON: %v", err))
	}
	if req.ABIVersion != ABIVersion {
		return failure(StatusInvalidArgument, fmt.Sprintf(
			"easysql: unsupported ABI version %d (expected %d)", req.ABIVersion, ABIVersion))
	}
	if strings.TrimSpace(req.Operation) == "" {
		return failure(StatusInvalidArgument, "easysql: operation must not be empty")
	}
	if len(req.Args) == 0 || string(req.Args) == "null" {
		return failure(StatusInvalidArgument, "easysql: args must be a JSON object")
	}

	data, err := dispatch(req.Operation, req.Args)
	if err != nil {
		return errorResponse(err)
	}
	return marshal(response{ABIVersion: ABIVersion, Status: StatusSuccess, Data: data})
}

// InternalFailure produces a valid response for failures in the thin cgo
// adapter before Execute can be entered.
func InternalFailure(message string) []byte {
	if strings.TrimSpace(message) == "" {
		message = "easysql: native adapter initialization failed"
	}
	return failure(StatusInternalError, message)
}

// InvalidArgumentFailure produces a valid response when the cgo adapter must
// reject a request before copying it into Go memory.
func InvalidArgumentFailure(message string) []byte {
	return failure(StatusInvalidArgument, message)
}

func dispatch(operation string, raw json.RawMessage) (any, error) {
	switch operation {
	case "applyRowFilter":
		var args applyRowFilterArgs
		if err := decodeArgs(raw, &args); err != nil {
			return nil, err
		}
		var opts []easysql.Option
		if args.Dialect != "" {
			opts = append(opts, easysql.WithDialect(args.Dialect))
		}
		if args.TableNames != nil {
			opts = append(opts, easysql.WithTableNames(args.TableNames...))
		}
		if args.TableRegexps != nil {
			opts = append(opts, easysql.WithTableRegexp(args.TableRegexps...))
		}
		if args.DefaultDB != "" {
			opts = append(opts, easysql.WithDefaultDB(args.DefaultDB))
		}
		return easysql.ApplyRowFilter(args.SQL, args.WhereClause, opts...)

	case "bindCTEs":
		var args bindCTEsArgs
		if err := decodeArgs(raw, &args); err != nil {
			return nil, err
		}
		bindings := make([]easysql.CTEBinding, len(args.Bindings))
		for i, binding := range args.Bindings {
			bindings[i] = easysql.CTEBinding{Name: binding.Name, Query: binding.Query}
		}
		return easysql.BindCTEs(args.ConsumerSQL, bindings,
			easysql.WithBindCTEDialect(args.Dialect))

	case "lineageSourceColumns":
		var args lineageArgs
		if err := decodeArgs(raw, &args); err != nil {
			return nil, err
		}
		return easysql.LineageSourceColumns(args.SQL, lineageOptions(args)...)

	case "parseColumns":
		var args lineageArgs
		if err := decodeArgs(raw, &args); err != nil {
			return nil, err
		}
		return easysql.ParseColumns(args.SQL, lineageOptions(args)...)

	case "referencedColumns":
		var args lineageArgs
		if err := decodeArgs(raw, &args); err != nil {
			return nil, err
		}
		return easysql.ReferencedColumns(args.SQL, lineageOptions(args)...)

	case "referencedColumnUsages":
		var args lineageArgs
		if err := decodeArgs(raw, &args); err != nil {
			return nil, err
		}
		uses, err := easysql.ReferencedColumnUsages(args.SQL, lineageOptions(args)...)
		if err != nil {
			return nil, err
		}
		type columnUse struct {
			Table  string `json:"table"`
			Column string `json:"column"`
			Clause string `json:"clause"`
		}
		result := make([]columnUse, len(uses))
		for i, use := range uses {
			result[i] = columnUse{Table: use.Table, Column: use.Column, Clause: string(use.Clause)}
		}
		return result, nil

	case "rewriteTableReferences":
		var args rewriteTableReferencesArgs
		if err := decodeArgs(raw, &args); err != nil {
			return nil, err
		}
		return easysql.RewriteTableReferences(args.SQL, tableRewrites(args.Specs),
			easysql.WithRewriteDialect(args.Dialect),
			easysql.StripMatchCatalogs(args.StripMatchCatalogs...))

	default:
		return nil, fmt.Errorf("easysql: unknown operation %q", operation)
	}
}

func tableRewrites(specs []tableRewrite) []easysql.TableRewrite {
	result := make([]easysql.TableRewrite, len(specs))
	for i, spec := range specs {
		result[i].MatchKey = spec.MatchKey
		if spec.Inline != nil {
			inline := toTableRef(*spec.Inline)
			result[i].Inline = &inline
		}
		if spec.Union != nil {
			branches := make([]easysql.TableRef, len(spec.Union.Branches))
			for j, branch := range spec.Union.Branches {
				branches[j] = toTableRef(branch)
			}
			result[i].Union = &easysql.UnionRewrite{
				TableAlias: spec.Union.TableAlias,
				Columns:    spec.Union.Columns,
				Branches:   branches,
			}
		}
	}
	return result
}

func toTableRef(ref tableRef) easysql.TableRef {
	return easysql.TableRef{Catalog: ref.Catalog, Schema: ref.Schema, Table: ref.Table}
}

func decodeArgs(raw json.RawMessage, dst any) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("easysql: invalid operation arguments: %w", err)
	}
	return nil
}

func lineageOptions(args lineageArgs) []easysql.LineageOption {
	opts := []easysql.LineageOption{easysql.WithLineageDialect(args.Dialect)}
	if args.Metadata != nil {
		opts = append(opts, easysql.WithLineageMetadata(args.Metadata))
	}
	if args.Producer != "" {
		opts = append(opts, easysql.WithLineageProducer(args.Producer))
	}
	if args.Namespace != "" {
		opts = append(opts, easysql.WithLineageNamespace(args.Namespace))
	}
	return opts
}

func errorResponse(err error) []byte {
	status := StatusInvalidArgument
	switch {
	case errors.Is(err, easysql.ErrParse):
		status = StatusParseError
	case errors.Is(err, easysql.ErrUnsupported):
		status = StatusUnsupported
	case errors.Is(err, easysql.ErrInternal):
		status = StatusInternalError
	}
	return failure(status, err.Error())
}

func failure(status int32, message string) []byte {
	return marshal(response{ABIVersion: ABIVersion, Status: status, Error: message})
}

func marshal(value response) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		return []byte(`{"abiVersion":1,"status":4,"error":"easysql: response serialization failed"}`)
	}
	return data
}
