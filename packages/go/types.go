package easysql

const ABIVersion uint32 = 1

type ApplyRowFilterOptions struct {
	Dialect      string
	TableNames   []string
	TableRegexps []string
	DefaultDB    string
}

type CTEBinding struct {
	Name  string `json:"name"`
	Query string `json:"query"`
}

type BindCTEsOptions struct {
	Dialect string
}

type AnalysisOptions struct {
	Dialect   string
	Metadata  map[string][]string
	Producer  string
	Namespace string
}

type ColumnUse struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Clause string `json:"clause"`
}

type TableRef struct {
	Catalog string `json:"catalog,omitempty"`
	Schema  string `json:"schema,omitempty"`
	Table   string `json:"table"`
}

type UnionRewrite struct {
	TableAlias string     `json:"tableAlias,omitempty"`
	Columns    []string   `json:"columns"`
	Branches   []TableRef `json:"branches"`
}

type TableRewrite struct {
	MatchKey string        `json:"matchKey"`
	Inline   *TableRef     `json:"inline,omitempty"`
	Union    *UnionRewrite `json:"union,omitempty"`
}

type RewriteTableReferencesOptions struct {
	Dialect            string
	StripMatchCatalogs []string
}
