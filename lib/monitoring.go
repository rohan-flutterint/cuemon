package lib

import (
	"cuelang.org/go/cue/ast"
	_ "embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	//go:embed grafana.cue
	GrafanaCue string
	//go:embed mon.cue
	MonCue string
	//go:embed mon2grafana.cue
	Mon2GrafanaCue string
	//go:embed grafana2mon/meta.cue
	Meta2MonitoringCue string
	//go:embed grafana2mon/template.cue
	Template2MonitoringCue string
	//go:embed grafana2mon/panel.cue
	Panel2MonitoringCue string
	//go:embed grafana2mon/target.cue
	Target2MonitoringCue string
)

var matchIgnoredSymbols = regexp.MustCompile("[^a-zA-Z0-9]")
var matchUnderscores = regexp.MustCompile("_+")

func toFilename(s string) string {
	s = matchIgnoredSymbols.ReplaceAllString(s, "_")
	s = matchUnderscores.ReplaceAllString(s, "_")
	return strings.ToLower(s)
}

const (
	GrafanaField       = "Grafana"
	SchemaVersionField = "schemaVersion"
	TitleField         = "Title"
	CollapsedField     = "Collapsed"
	ColumnsField       = "Columns"
	HeightsField       = "Height"
	PanelField         = "Panel"
	PanelGridField     = "PanelGrid"
	MetricsField       = "Metrics"
	WidthField         = "Width"
	HeightField        = "Height"
	VariablesField     = "Variables"
	RowsField          = "Rows"
)

const (
	PanelVar      = "#Panel"
	TargetVar     = "#Target"
	MonitoringVar = "#Monitoring"
	RowVar        = "#Row"
	anyVar        = "_"
	variablesVar  = "variables"
)

const (
	CuemonPath = "github.com/sivukhin/cuemon"
	CuemonName = "cuemon"
)

type monitoringContext struct {
	context []byte
}

func (m monitoringContext) createPanel(panel JsonRaw[GrafanaPanel]) ([]ast.Decl, error) {
	panelDecls, err := CueConvert(PanelVar, []string{GrafanaCue, MonCue, Panel2MonitoringCue}, panel.Raw, m.context, false)
	if err != nil {
		return nil, fmt.Errorf("unable to convert panel '%v': %w", panel.Value.Title, err)
	}
	targetsDecls := make([]ast.Expr, 0)
	for i, target := range panel.Value.Targets {
		targetDecls, err := CueConvert(TargetVar, []string{GrafanaCue, MonCue, Target2MonitoringCue}, target, m.context, false)
		if err != nil {
			return nil, fmt.Errorf("unable to convert target #%v from panel '%v': %w", i, panel.Value.Title, err)
		}
		targetsDecls = append(targetsDecls, ast.NewStruct(AsAny(targetDecls)...))
	}
	if len(targetsDecls) > 0 {
		panelDecls = append(panelDecls, FieldIdent(MetricsField, ast.NewList(targetsDecls...)))
	}
	return panelDecls, nil
}

func (m monitoringContext) createRow(row GrafanaPanel, panels []JsonRaw[GrafanaPanel]) (*Row, error) {
	decls := []ast.Decl{FieldIdent(TitleField, ast.NewString(row.Title))}
	if row.Collapsed {
		decls = append(decls, FieldIdent(CollapsedField, ast.NewBool(true)))
	}

	grid := make([]Box, 0)
	for i, panel := range panels {
		box := panel.Value.GridPos
		box.Id = i
		grid = append(grid, box)
	}
	layout := AnalyzeGrid(grid)

	decls = append(decls, FieldIdent(ColumnsField, IntList(layout.Columns)))
	if len(layout.Heights) == 1 {
		decls = append(decls, FieldIdent(HeightsField, Int(layout.Heights[0])))
	} else {
		decls = append(decls, FieldIdent(HeightsField, IntList(layout.Heights)))
	}
	decls = append(decls, LineBreak())

	for _, id := range layout.Order {
		panel := panels[id]
		panelDecls, err := m.createPanel(panel)
		if err != nil {
			return nil, fmt.Errorf("unable to convert panel %v: %w", panel.Value.Title, err)
		}
		if override, ok := layout.Overrides[id]; ok {
			decls = append(decls, FieldIdent(PanelGridField, ast.NewStruct(ast.NewIdent(panel.Value.Title), ast.NewStruct(
				FieldIdent(WidthField, Int(override.Width)),
				FieldIdent(HeightField, Int(override.Height)),
			))))
		}
		decls = append(decls, FieldIdent(PanelField, ast.NewStruct(
			ast.NewIdent(panel.Value.Title), ast.NewStruct(AsAny(panelDecls)...),
		)))
		decls = append(decls, LineBreak())
	}
	return &Row{Name: toFilename(row.Title), Src: decls}, nil
}

type Row struct {
	Name string
	Src  []ast.Decl
}

type Monitoring struct {
	Meta      []ast.Decl
	Variables []ast.Decl
	Rows      []*Row
}

func (m monitoringContext) createVariable(template Templating) ([]ast.Decl, error) {
	variable, err := CueConvert(anyVar, []string{GrafanaCue, MonCue, Template2MonitoringCue}, template.Raw, m.context, false)
	if err != nil {
		return nil, fmt.Errorf("unable to convert variable '%v': %v", template.Value.Name, err)
	}
	return []ast.Decl{FieldIdent(template.Value.Name, ast.NewStruct(AsAny(variable)...))}, nil
}

func (m monitoringContext) creteMeta(grafana Grafana) ([]ast.Decl, error) {
	meta, err := CueConvert(anyVar, []string{GrafanaCue, Meta2MonitoringCue}, grafana.Raw, m.context, true)
	if err != nil {
		return nil, fmt.Errorf("unable to convert meta: %w", err)
	}
	return meta, nil
}

func ExtractRows(grafana Grafana) []GrafanaPanel {
	title, collapsed := "", false
	i := 0
	rows := make([]GrafanaPanel, 0)
	for i < len(grafana.Value.Panels) {
		current := grafana.Value.Panels[i]
		if current.Value.Type == "row" {
			title = current.Value.Title
			collapsed = current.Value.Collapsed
			i += 1
		}
		var panels []JsonRaw[GrafanaPanel]
		if collapsed {
			panels = current.Value.Panels
		} else {
			startI := i
			for i < len(grafana.Value.Panels) && grafana.Value.Panels[i].Value.Type != "row" {
				i += 1
			}
			panels = grafana.Value.Panels[startI:i]
		}
		rows = append(rows, GrafanaPanel{Title: title, Collapsed: collapsed, Panels: panels})
	}
	return rows
}

func (m monitoringContext) createDashboard(grafana Grafana) (*Monitoring, error) {
	meta, err := m.creteMeta(grafana)
	if err != nil {
		return nil, err
	}
	variables := make([]ast.Decl, 0)
	for _, template := range grafana.Value.Templating.List {
		variable, err := m.createVariable(template)
		if err != nil {
			return nil, err
		}
		variables = append(variables, variable...)
	}
	rows := make([]*Row, 0)
	for _, grafanaRow := range ExtractRows(grafana) {
		row, err := m.createRow(grafanaRow, grafanaRow.Panels)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return &Monitoring{Meta: meta, Variables: variables, Rows: rows}, nil
}

type FileEntry struct {
	Path    string
	Content string
}

func MonitoringFiles(module, output string, grafana Grafana) ([]FileEntry, error) {
	m := monitoringContext{context: []byte(fmt.Sprintf(`SchemaVersion: %v`, grafana.Value.SchemaVersion))}
	monitoring, err := m.createDashboard(grafana)
	if err != nil {
		return nil, fmt.Errorf("unable to create monitoring configuration: %w", err)
	}
	packageName := PackageName(module)
	files := make([]FileEntry, 0)
	files = append(files, FileEntry{path.Join(output, "cue.mod", "module.cue"), fmt.Sprintf("module: \"%v\"\n", module)})
	files = append(files, FileEntry{path.Join(output, "cue.mod", "pkg", "github.com", "sivukhin", "cuemon", "grafana.cue"), GrafanaCue})
	files = append(files, FileEntry{path.Join(output, "cue.mod", "pkg", "github.com", "sivukhin", "cuemon", "mon.cue"), MonCue})
	files = append(files, FileEntry{path.Join(output, "cue.mod", "pkg", "github.com", "sivukhin", "cuemon", "mon2grafana.cue"), Mon2GrafanaCue})

	rowVars := make([]string, 0)
	dashboardImports := []string{CuemonPath, fmt.Sprintf("%v:%v", module, variablesVar)}
	for _, row := range monitoring.Rows {
		dashboardImports = append(dashboardImports, fmt.Sprintf("%v:%v", module, row.Name))
		rowVars = append(rowVars, row.Name)
	}
	dashboardDecls := []ast.Decl{
		Package(packageName),
		Imports(dashboardImports...),
		ast.NewIdent(CuemonName),
		LineBreak(),
	}
	dashboardDecls = append(dashboardDecls, monitoring.Meta...)
	dashboardDecls = append(dashboardDecls, FieldIdent(VariablesField, ast.NewIdent(variablesVar)))
	dashboardDecls = append(dashboardDecls, FieldIdent(RowsField, IdentList(rowVars)))

	files = append(files, FileEntry{path.Join(output, "dashboard.cue"), FormatDecls(dashboardDecls)})
	files = append(files, FileEntry{path.Join(output, "variables.cue"), FormatDecls(
		append([]ast.Decl{Package(packageName)}, monitoring.Variables...),
	)})
	for _, row := range monitoring.Rows {
		rowDecls := append(
			[]ast.Decl{Package(packageName), Imports(CuemonPath), ast.NewSel(ast.NewIdent(CuemonName), RowVar)},
			row.Src...,
		)
		files = append(files, FileEntry{path.Join(output, "rows", fmt.Sprintf("%v.cue", row.Name)), FormatDecls(rowDecls)})
	}
	return files, nil
}

type FileEntryUpdated struct {
	Path       string
	Content    []byte
	BeforePart []byte
	AfterPart  []byte
}

func UpdateFiles(output string, panel JsonRaw[GrafanaPanel]) ([]FileEntryUpdated, error) {
	targets := make(map[string]ast.Node, 0)
	versions := make([]int, 0)
	_ = filepath.WalkDir(output, func(path string, d fs.DirEntry, err error) error {
		if d == nil {
			return fmt.Errorf("directory %v not found", output)
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".cue") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		cueAst, err := CueAst(string(content))
		if err != nil {
			return err
		}
		_, versionNode := findField(cueAst.Decls, ast.NewIdent(GrafanaField), ast.NewIdent(SchemaVersionField))
		if versionNode != nil {
			version, err := strconv.Atoi(versionNode.(*ast.BasicLit).Value)
			if err != nil {
				return fmt.Errorf("unable to parse Grafana schema version: %w", err)
			}
			versions = append(versions, version)
		}
		target, _ := findField(cueAst.Decls, ast.NewIdent(PanelField), ast.NewString(panel.Value.Title))
		if target != nil {
			targets[path] = target
		}
		return nil
	})
	if len(Unique(versions)) != 1 {
		return nil, fmt.Errorf("unable to find single Grafana schema version: %v", versions)
	}
	version := versions[0]
	m := monitoringContext{context: []byte(fmt.Sprintf(`SchemaVersion: %v`, version))}
	panelDecls, err := m.createPanel(panel)
	if err != nil {
		return nil, fmt.Errorf("unable to convert panel %v: %w", panel.Value.Title, err)
	}
	panelStruct := FieldIdent(PanelField, ast.NewStruct(
		ast.NewIdent(panel.Value.Title), ast.NewStruct(AsAny(panelDecls)...),
	))
	updates := make([]FileEntryUpdated, 0)
	for targetPath, target := range targets {
		content, err := os.ReadFile(targetPath)
		if err != nil {
			return nil, err
		}
		cutted, oldPart, newPart, err := CutNode(content, target, panelStruct)
		if err != nil {
			return nil, err
		}
		updates = append(updates, FileEntryUpdated{Path: targetPath, Content: cutted, BeforePart: oldPart, AfterPart: newPart})
	}
	return updates, nil
}