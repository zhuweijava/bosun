package sched

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"bosun.org/cmd/bosun/conf"
	"bosun.org/cmd/bosun/expr"
	"bosun.org/cmd/bosun/expr/parse"
	"bosun.org/opentsdb"
)

type Context struct {
	*State
	Alert *conf.Alert

	schedule    *Schedule
	runHistory  *RunHistory
	Attachments []*conf.Attachment
}

func (s *Schedule) Data(rh *RunHistory, st *State, a *conf.Alert, isEmail bool) *Context {
	c := Context{
		State:      st,
		Alert:      a,
		schedule:   s,
		runHistory: rh,
	}
	if isEmail {
		c.Attachments = make([]*conf.Attachment, 0)
	}
	return &c
}

type unknownContext struct {
	Time  time.Time
	Name  string
	Group expr.AlertKeys

	schedule *Schedule
}

func (s *Schedule) unknownData(t time.Time, name string, group expr.AlertKeys) *unknownContext {
	return &unknownContext{
		Time:     t,
		Group:    group,
		Name:     name,
		schedule: s,
	}
}

// Ack returns the URL to acknowledge an alert.
func (c *Context) Ack() string {
	return c.makeLink("/action", &url.Values{
		"type": []string{"ack"},
		"key":  []string{c.Alert.Name + c.State.Group.String()},
	})
}

// HostView returns the URL to the host view page.
func (c *Context) HostView(host string) string {
	return c.makeLink("/host", &url.Values{
		"time": []string{"1d-ago"},
		"host": []string{host},
	})
}

func (c *Context) makeLink(path string, v *url.Values) string {
	u := url.URL{
		Scheme:   "http",
		Host:     c.schedule.Conf.Hostname,
		Path:     path,
		RawQuery: v.Encode(),
	}
	return u.String()
}

func (c *Context) Expr(v string) string {
	p := url.Values{}
	p.Add("expr", base64.StdEncoding.EncodeToString([]byte(opentsdb.ReplaceTags(v, c.Group))))
	return c.makeLink("/expr", &p)
}

func (c *Context) Rule() (string, error) {
	t, err := c.schedule.Conf.AlertTemplateStrings()
	if err != nil {
		return "", err
	}
	p := url.Values{}
	adef := base64.StdEncoding.EncodeToString([]byte(t.Alerts[c.Alert.Name]))
	tdef := base64.StdEncoding.EncodeToString([]byte(t.Templates[c.Alert.Template.Name]))
	//There might be something better when we tie the notifications to evaluation time issue #395
	time := time.Now().UTC()
	p.Add("alert", adef)
	p.Add("template", tdef)
	p.Add("fromDate", time.Format("2006-01-02"))
	p.Add("fromTime", time.Format("15:04"))
	p.Add("template_group", c.Group.Tags())
	return c.makeLink("/rule", &p), nil
}

func (s *Schedule) ExecuteBody(rh *RunHistory, a *conf.Alert, st *State, isEmail bool) ([]byte, []*conf.Attachment, error) {
	t := a.Template
	if t == nil || t.Body == nil {
		return nil, nil, nil
	}
	c := s.Data(rh, st, a, isEmail)
	buf := new(bytes.Buffer)
	err := t.Body.Execute(buf, c)
	return buf.Bytes(), c.Attachments, err
}

func (s *Schedule) ExecuteSubject(rh *RunHistory, a *conf.Alert, st *State) ([]byte, error) {
	t := a.Template
	if t == nil || t.Subject == nil {
		return nil, nil
	}
	buf := new(bytes.Buffer)
	err := t.Subject.Execute(buf, s.Data(rh, st, a, false))
	return bytes.Join(bytes.Fields(buf.Bytes()), []byte(" ")), err
}

var error_body = template.Must(template.New("body_error_template").Parse(`
	<p>There was a runtime error processing alert {{.State.AlertKey}} using the {{.Alert.Template.Name}} template. The following errors occurred:</p>
	{{if .Serr}}
		<p>Subject: {{.Serr}}</p>
	{{end}}
	{{if .Berr}}
		<p>Body: {{.Berr}}</p>
	{{end}}
	<p>Use <a href="{{.Rule}}">this link</a> to the rule page to correct this.</p>
	<h2>Generic Alert Information</h2>
	<p>Status: {{.Last.Status}}</p>
	<p>Alert: {{.State.AlertKey}}</p>
	<h3>Computations</h3>
	<table>
		<tr>
			<th style="text-align:left">Expression</th>
			<th style="text-align:left">Value</th>
		</tr>
	{{range .Computations}}
		<tr>
			<td style="text-align:left">{{.Text}}</td>
			<td style="text-align:left">{{.Value}}</td>
		</tr>
	{{end}}</table>`))

func (s *Schedule) ExecuteBadTemplate(s_err, b_err error, rh *RunHistory, a *conf.Alert, st *State) (subject, body []byte, err error) {
	sub := "error: template rendering error in the "
	if s_err != nil {
		sub += "subject"
	}
	if s_err != nil && b_err != nil {
		sub += " and "
	}
	if b_err != nil {
		sub += "body"
	}
	sub += fmt.Sprintf(" for alert %v", st.AlertKey())
	c := struct {
		Serr, Berr error
		*Context
	}{
		Serr:    s_err,
		Berr:    b_err,
		Context: s.Data(rh, st, a, true),
	}
	buf := new(bytes.Buffer)
	error_body.Execute(buf, c)
	return []byte(sub), buf.Bytes(), nil
}

func (c *Context) eval(v interface{}, filter bool, series bool, autods int) (expr.ResultSlice, string, error) {
	var e *expr.Expr
	var err error
	switch v := v.(type) {
	case string:
		e, err = expr.New(v, c.schedule.Conf.Funcs())
	case *expr.Expr:
		e = v
	default:
		return nil, "", fmt.Errorf("expected string or expression, got %T (%v)", v, v)
	}
	if err != nil {
		return nil, "", fmt.Errorf("%v: %v", v, err)
	}
	if filter {
		e, err = expr.New(opentsdb.ReplaceTags(e.Text, c.State.Group), c.schedule.Conf.Funcs())
		if err != nil {
			return nil, "", err
		}
	}
	if series && e.Root.Return() != parse.TypeSeries {
		return nil, "", fmt.Errorf("egraph: requires an expression that returns a series")
	}
	res, _, err := e.Execute(c.runHistory.Context, c.runHistory.GraphiteContext, c.schedule.Conf.LogstashElasticHost, nil, c.runHistory.Start, autods, c.Alert.UnjoinedOK, c.schedule.Search, c.schedule.Conf.AlertSquelched(c.Alert))
	if err != nil {
		return nil, "", fmt.Errorf("%s: %v", v, err)
	}
	return res.Results, e.String(), nil
}

// Lookup returns the value for a key in the lookup table for the context's tagset.
func (c *Context) Lookup(table, key string) (string, error) {
	return c.LookupAll(table, key, c.Group)
}

func (c *Context) LookupAll(table, key string, group interface{}) (string, error) {
	var t opentsdb.TagSet
	switch v := group.(type) {
	case string:
		var err error
		t, err = opentsdb.ParseTags(v)
		if err != nil {
			return "", err
		}
	case opentsdb.TagSet:
		t = v
	}
	l, ok := c.schedule.Conf.Lookups[table]
	if !ok {
		return "", fmt.Errorf("unknown lookup table %v", table)
	}
	if v, ok := l.ToExpr().Get(key, t); ok {
		return v, nil
	}
	return "", fmt.Errorf("no entry for key %v in table %v for tagset %v", key, table, c.Group)
}

// Eval executes the given expression and returns a value with corresponding
// tags to the context's tags. If no such result is found, the first result with
// nil tags is returned. If no such result is found, nil is returned.
func (c *Context) Eval(v interface{}) (interface{}, error) {
	res, _, err := c.eval(v, true, false, 0)
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("no results returned")
	}
	// TODO: don't choose a random result, make sure there's exactly 1
	return res[0].Value, nil
}

// EvalAll returns the executed expression.
func (c *Context) EvalAll(v interface{}) (interface{}, error) {
	res, _, err := c.eval(v, false, false, 0)
	return res, err
}

func (c *Context) IsEmail() bool {
	return c.Attachments != nil
}

func (c *Context) graph(v interface{}, filter bool) (interface{}, error) {
	res, title, err := c.eval(v, filter, true, 1000)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	const width = 800
	const height = 600
	if c.IsEmail() {
		err := c.schedule.ExprPNG(nil, &buf, width, height, res, title, c.runHistory.Start)
		if err != nil {
			return nil, err
		}
		name := fmt.Sprintf("%d.png", len(c.Attachments)+1)
		c.Attachments = append(c.Attachments, &conf.Attachment{
			Data:        buf.Bytes(),
			Filename:    name,
			ContentType: "image/png",
		})
		return template.HTML(fmt.Sprintf(`<img alt="%s" src="cid:%s" />`,
			template.HTMLEscapeString(fmt.Sprint(v)),
			name,
		)), nil
	}
	if err := c.schedule.ExprSVG(nil, &buf, width, height, res, title, time.Now().UTC()); err != nil {
		return nil, err
	}
	return template.HTML(buf.String()), nil
}

func (c *Context) Graph(v interface{}) (interface{}, error) {
	return c.graph(v, true)
}

func (c *Context) GraphAll(v interface{}) (interface{}, error) {
	return c.graph(v, false)
}

func (c *Context) GetMeta(metric, name string, v interface{}) (interface{}, error) {
	var t opentsdb.TagSet
	switch v := v.(type) {
	case string:
		var err error
		t, err = opentsdb.ParseTags(v)
		if err != nil {
			return t, err
		}
	case opentsdb.TagSet:
		t = v
	}
	meta := c.schedule.GetMetadata(metric, t)
	if name == "" {
		return meta, nil
	}
	for _, m := range meta {
		if m.Name == name {
			return m.Value, nil
		}
	}
	return nil, nil
}

func (c *Context) LeftJoin(q ...interface{}) (interface{}, error) {
	if len(q) < 2 {
		return nil, fmt.Errorf("need at least two expressions, got %v", len(q))
	}
	matrix := make([][]*expr.Result, 0)
	results := make([][]*expr.Result, len(q))
	for col, v := range q {
		res, _, err := c.eval(v, false, false, 0)
		if err != nil {
			return nil, err
		}
		results[col] = res
	}
	for row, first := range results[0] {
		matrix = append(matrix, make([]*expr.Result, len(q)))
		matrix[row][0] = first
		for col, res := range results[1:] {
			for _, r := range res {
				if first.Group.Subset(r.Group) {
					matrix[row][col+1] = r
					break
				}
				// Fill emtpy cells with NaN Value, so calling .Valie is not a nil pointer dereference
				matrix[row][col+1] = &expr.Result{Value: expr.Number(math.NaN())}
			}
		}
	}
	return matrix, nil
}

func (c *Context) HTTPGet(url string) string {
	resp, err := http.Get(url)
	if err != nil {
		return err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Sprintf("%v: returned %v", url, resp.Status)
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err.Error()
	}
	return string(body)
}

func (c *Context) HTTPPost(url, bodyType, data string) string {
	resp, err := http.Post(url, bodyType, bytes.NewBufferString(data))
	if err != nil {
		return err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Sprintf("%v: returned %v", url, resp.Status)
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err.Error()
	}
	return string(body)
}

func (c *Context) LSQuery(index_root, filter, sduration, eduration string, size int) (interface{}, error) {
	var ks []string
	for k, v := range c.Group {
		ks = append(ks, k+":"+v)
	}
	return c.LSQueryAll(index_root, strings.Join(ks, ","), filter, sduration, eduration, size)
}

func (c *Context) LSQueryAll(index_root, keystring, filter, sduration, eduration string, size int) (interface{}, error) {
	service, s, _, err := expr.LSBaseQuery(time.Now(), c.schedule.Conf.LogstashElasticHost, index_root, keystring, filter, sduration, eduration, size)
	if err != nil {
		return nil, err
	}
	results, err := service.SearchSource(s).Do()
	if err != nil {
		return nil, err
	}
	r := make([]interface{}, len(results.Hits.Hits))
	for i, h := range results.Hits.Hits {
		var err error
		err = json.Unmarshal(*h.Source, &r[i])
		if err != nil {
			return nil, err
		}
	}
	return r, nil
}
