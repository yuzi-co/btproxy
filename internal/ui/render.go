package ui

import (
	"strings"
)

// esc escapes text for HTML element content and quoted attribute values.
func esc(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&#34;")
		case '\'':
			b.WriteString("&#39;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

const style = `<style>
:root{color-scheme:light dark;--bg:#fff;--fg:#1b1b1b;--mut:#666;--line:#ddd;--acc:#1565c0;--err:#c62828;--warn:#fff4e5}
@media(prefers-color-scheme:dark){:root{--bg:#161616;--fg:#e8e8e8;--mut:#9a9a9a;--line:#333;--acc:#64a0f0;--err:#ef6b6b;--warn:#3a2e14}}
body{font:15px/1.45 system-ui,sans-serif;background:var(--bg);color:var(--fg);margin:0 auto;max-width:44rem;padding:1rem}
h1{font-size:1.4rem;margin:.2rem 0 1rem}h2{font-size:1.05rem;margin:1.6rem 0 .5rem;border-bottom:1px solid var(--line);padding-bottom:.2rem}
table{border-collapse:collapse;width:100%}td{padding:.15rem .5rem .15rem 0;vertical-align:top}td:first-child{color:var(--mut);white-space:nowrap;width:10rem}
.f{margin:.7rem 0}.f label{display:block;font-weight:600}.f small{display:block;color:var(--mut)}
input[type=text],input[type=number],input[type=password],select{box-sizing:border-box;width:100%;padding:.35rem;font:inherit;background:var(--bg);color:var(--fg);border:1px solid var(--line);border-radius:4px}
input:disabled,select:disabled{opacity:.6}.row{display:flex;gap:.4rem}.row input{flex:1}
.e{color:var(--err)}.n{background:var(--warn);padding:.5rem .7rem;border-radius:4px;margin:.6rem 0}
button{font:inherit;padding:.35rem .9rem;border-radius:4px;border:1px solid var(--line);background:var(--bg);color:var(--fg);cursor:pointer}
button.p{background:var(--acc);border-color:var(--acc);color:#fff}
</style>`

const script = `<script>
function show(id,b){var i=document.getElementById(id);i.type=i.type=="password"?"text":"password";b.textContent=i.type=="password"?"Show":"Hide"}
function gen(id){var a=new Uint8Array(32);crypto.getRandomValues(a);var i=document.getElementById(id);i.value=btoa(String.fromCharCode.apply(null,a));i.type="text"}
setInterval(function(){fetch("/status.json",{cache:"no-store"}).then(function(r){return r.ok?r.json():null}).then(function(m){
if(!m)return;for(var k in m){var e=document.querySelector('[data-k="'+k+'"]');if(e)e.textContent=m[k]}}).catch(function(){})},5000)
</script>`

const restartPage = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>btproxy</title>` + style + `</head><body>
<h1>Saved</h1><p>btproxy is restarting with the new settings. This page reloads once it is back.</p><p><a href="/">Back to the settings</a></p>
<script>setTimeout(function p(){fetch("/status.json",{cache:"no-store"}).then(function(r){if(r.ok)location="/";else setTimeout(p,1000)}).catch(function(){setTimeout(p,1000)})},1500)</script>
</body></html>`

// render builds the page. values, when not nil, replace the fields' values (a form sent back with errors).
func (s *Server) render(p Page, notice string, values map[string]string) *Response {
	var b strings.Builder
	w := func(ss ...string) {
		for _, x := range ss {
			b.WriteString(x)
		}
	}
	w(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>`,
		esc(p.Title), `</title>`, style, `</head><body><h1>`, esc(p.Title), `</h1>`)
	for _, n := range []string{p.Notice, notice, p.ReadOnly} {
		if n != "" {
			w(`<div class="n">`, esc(n), `</div>`)
		}
	}
	w(`<h2>Status</h2><table>`)
	for _, kv := range p.Status {
		w(`<tr><td>`, esc(kv.Key), `</td><td data-k="`, esc(kv.Key), `">`, esc(kv.Value), `</td></tr>`)
	}
	w(`</table><form method="post" action="/save"><input type="hidden" name="csrf" value="`, s.csrf, `">`)
	for _, sec := range p.Sections {
		w(`<h2>`, esc(sec.Title), `</h2>`)
		for _, f := range sec.Fields {
			v := f.Value
			if nv, ok := values[f.Name]; ok {
				v = nv
			}
			s.field(w, f, v, p.ReadOnly != "")
		}
	}
	if p.ReadOnly == "" {
		w(`<p><button class="p" type="submit">Save and restart</button></p>`)
	}
	w(`</form>`, script, `</body></html>`)
	return &Response{Status: 200, Type: "text/html; charset=utf-8", Body: []byte(b.String())}
}

func (s *Server) field(w func(...string), f Field, v string, readOnly bool) {
	id := "f-" + esc(f.Name)
	dis := ""
	if f.Locked != "" || readOnly {
		dis = " disabled"
	}
	attrs := ` id="` + id + `" name="` + esc(f.Name) + `"` + dis
	if f.Kind == Bool {
		checked := ""
		if v == "true" {
			checked = " checked"
		}
		w(`<div class="f"><label><input type="checkbox" value="true"`, attrs, checked, `> `, esc(f.Label), `</label>`)
	} else {
		w(`<div class="f"><label for="`, id, `">`, esc(f.Label), `</label>`)
		switch f.Kind {
		case Select:
			w(`<select`, attrs, `>`)
			for _, o := range f.Options {
				sel := ""
				if o == v {
					sel = " selected"
				}
				w(`<option`, sel, `>`, esc(o), `</option>`)
			}
			w(`</select>`)
		case Secret:
			// The stored value never goes to the browser; only a new one the form just sent comes back.
			shown, placeholder := v, f.Placeholder
			if v == f.Value {
				shown = ""
			}
			if f.Value != "" {
				placeholder = "set; leave empty to keep it"
			}
			w(`<div class="row"><input type="password" autocomplete="off" value="`, esc(shown), `" placeholder="`,
				esc(placeholder), `"`, attrs, `><button type="button" onclick="show('`, id, `',this)">Show</button>`)
			if dis == "" {
				w(`<button type="button" onclick="gen('`, id, `')">Generate</button>`)
			}
			w(`</div>`)
			if f.Value != "" && dis == "" {
				checked := ""
				if v == "" {
					checked = " checked"
				}
				w(`<label><input type="checkbox" value="true" name="`, esc(f.Name), secretClear, `"`, checked, `> Remove</label>`)
			}
		default:
			typ := "text"
			if f.Kind == Number {
				typ = "number"
			}
			w(`<input type="`, typ, `" value="`, esc(v), `" placeholder="`, esc(f.Placeholder), `"`, attrs, `>`)
		}
	}
	if f.Help != "" {
		w(`<small>`, esc(f.Help), `</small>`)
	}
	if f.Locked != "" {
		w(`<small>`, esc(f.Locked), `</small>`)
	}
	if f.Error != "" {
		w(`<small class="e">`, esc(f.Error), `</small>`)
	}
	w(`</div>`)
}
