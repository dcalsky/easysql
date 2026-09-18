package easysql

import (
	"encoding/json"
	"fmt"
)

type requestEnvelope struct {
	ABIVersion uint32 `json:"abiVersion"`
	Operation  string `json:"operation"`
	Args       any    `json:"args"`
}

type responseEnvelope struct {
	ABIVersion uint32          `json:"abiVersion"`
	Status     Status          `json:"status"`
	Data       json.RawMessage `json:"data"`
	Error      string          `json:"error"`
}

func (client *Client) ApplyRowFilter(sql, whereClause string, options ...ApplyRowFilterOptions) (string, error) {
	option, err := optional("ApplyRowFilter", options)
	if err != nil {
		return "", err
	}
	args := struct {
		SQL          string   `json:"sql"`
		WhereClause  string   `json:"whereClause"`
		Dialect      string   `json:"dialect,omitempty"`
		TableNames   []string `json:"tableNames,omitempty"`
		TableRegexps []string `json:"tableRegexps,omitempty"`
		DefaultDB    string   `json:"defaultDB,omitempty"`
	}{sql, whereClause, option.Dialect, option.TableNames, option.TableRegexps, option.DefaultDB}
	return call[string](client, "applyRowFilter", args)
}

func (client *Client) BindCTEs(consumerSQL string, bindings []CTEBinding, options ...BindCTEsOptions) (string, error) {
	option, err := optional("BindCTEs", options)
	if err != nil {
		return "", err
	}
	args := struct {
		ConsumerSQL string       `json:"consumerSQL"`
		Bindings    []CTEBinding `json:"bindings"`
		Dialect     string       `json:"dialect,omitempty"`
	}{consumerSQL, bindings, option.Dialect}
	return call[string](client, "bindCTEs", args)
}

func (client *Client) LineageSourceColumns(sql string, options ...AnalysisOptions) (map[string][]string, error) {
	return analysisCall[map[string][]string](client, "lineageSourceColumns", sql, options)
}

func (client *Client) ParseColumns(sql string, options ...AnalysisOptions) ([]string, error) {
	return analysisCall[[]string](client, "parseColumns", sql, options)
}

func (client *Client) ReferencedColumns(sql string, options ...AnalysisOptions) (map[string][]string, error) {
	return analysisCall[map[string][]string](client, "referencedColumns", sql, options)
}

func (client *Client) ReferencedColumnUsages(sql string, options ...AnalysisOptions) ([]ColumnUse, error) {
	return analysisCall[[]ColumnUse](client, "referencedColumnUsages", sql, options)
}

func (client *Client) RewriteTableReferences(sql string, specs []TableRewrite, options ...RewriteTableReferencesOptions) (string, error) {
	option, err := optional("RewriteTableReferences", options)
	if err != nil {
		return "", err
	}
	args := struct {
		SQL                string         `json:"sql"`
		Specs              []TableRewrite `json:"specs"`
		Dialect            string         `json:"dialect,omitempty"`
		StripMatchCatalogs []string       `json:"stripMatchCatalogs,omitempty"`
	}{sql, specs, option.Dialect, option.StripMatchCatalogs}
	return call[string](client, "rewriteTableReferences", args)
}

func analysisCall[T any](client *Client, operation, sql string, options []AnalysisOptions) (T, error) {
	option, err := optional(operation, options)
	if err != nil {
		var zero T
		return zero, err
	}
	args := struct {
		SQL       string              `json:"sql"`
		Dialect   string              `json:"dialect,omitempty"`
		Metadata  map[string][]string `json:"metadata,omitempty"`
		Producer  string              `json:"producer,omitempty"`
		Namespace string              `json:"namespace,omitempty"`
	}{sql, option.Dialect, option.Metadata, option.Producer, option.Namespace}
	return call[T](client, operation, args)
}

func call[T any](client *Client, operation string, args any) (T, error) {
	var zero T
	request, err := json.Marshal(requestEnvelope{ABIVersion: ABIVersion, Operation: operation, Args: args})
	if err != nil {
		return zero, fmt.Errorf("easysql %s: encode request: %w", operation, err)
	}
	responseJSON, err := client.Execute(request)
	if err != nil {
		return zero, err
	}
	var response responseEnvelope
	if err := json.Unmarshal(responseJSON, &response); err != nil {
		return zero, fmt.Errorf("easysql %s: decode response: %w", operation, err)
	}
	if response.ABIVersion != ABIVersion {
		return zero, fmt.Errorf("easysql %s: response ABI %d, expected %d", operation, response.ABIVersion, ABIVersion)
	}
	if response.Status != StatusSuccess {
		return zero, &Error{Operation: operation, Status: response.Status, Message: response.Error}
	}
	if err := json.Unmarshal(response.Data, &zero); err != nil {
		return zero, fmt.Errorf("easysql %s: decode response data: %w", operation, err)
	}
	return zero, nil
}

func optional[T any](operation string, options []T) (T, error) {
	var zero T
	if len(options) > 1 {
		return zero, fmt.Errorf("easysql: %s accepts at most one options value", operation)
	}
	if len(options) == 1 {
		return options[0], nil
	}
	return zero, nil
}
