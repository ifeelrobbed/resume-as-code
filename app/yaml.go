package main

import (
	"bytes"
	"html/template"
	"regexp"

	"gopkg.in/yaml.v3"
)

// specDoc mirrors the Kubernetes object shape (apiVersion/kind/metadata/spec)
// the resume page's "spec" view renders - a deliberate nod to the site's own
// GitOps subject matter. It's a projection of Resume that drops the fields
// that only make sense in prose (Bullets, Blurb, SubRoles) so the YAML stays
// legible instead of dumping the entire rendered view as text.
type specDoc struct {
	APIVersion string       `yaml:"apiVersion"`
	Kind       string       `yaml:"kind"`
	Metadata   specMetadata `yaml:"metadata"`
	Spec       specBody     `yaml:"spec"`
}

type specMetadata struct {
	Name string `yaml:"name"`
}

type specBody struct {
	Experience []specExperience `yaml:"experience"`
	Education  []specEducation  `yaml:"education"`
}

type specExperience struct {
	Role       string   `yaml:"role"`
	Company    string   `yaml:"company"`
	From       string   `yaml:"from"`
	To         string   `yaml:"to"`
	Skills     []string `yaml:"skills,omitempty"`
	Promotions int      `yaml:"promotions,omitempty"`
}

type specEducation struct {
	Degree string `yaml:"degree"`
	School string `yaml:"school"`
	Year   string `yaml:"year"`
}

func buildSpecDoc(r Resume) specDoc {
	doc := specDoc{
		APIVersion: "resume/v1",
		Kind:       "Career",
		Metadata:   specMetadata{Name: "rob-cameron"},
	}
	for _, e := range r.Experience {
		doc.Spec.Experience = append(doc.Spec.Experience, specExperience{
			Role:       e.Title,
			Company:    e.Company,
			From:       e.From,
			To:         e.To,
			Skills:     e.Skills,
			Promotions: e.Promotions,
		})
	}
	for _, ed := range r.Education {
		doc.Spec.Education = append(doc.Spec.Education, specEducation{
			Degree: ed.Degree,
			School: ed.School,
			Year:   ed.Year,
		})
	}
	return doc
}

// yamlLine matches a marshaled yaml.v3 line into (indent, optional "- ",
// key, ": " separator, value). Value is empty when the key introduces a
// nested block (e.g. "metadata:") rather than a scalar.
var yamlLine = regexp.MustCompile(`^(\s*)(- )?([A-Za-z][\w]*):(?: (.*))?$`)

// yamlListItem matches a plain "- value" block-list line (no key).
var yamlListItem = regexp.MustCompile(`^(\s*)- (.+)$`)

// specHTML marshals v to YAML and re-renders it with the same key/value
// coloring used across the rest of the site: keys in the accent color,
// values in the secondary "syntax" color, punctuation muted. This is
// deliberately hand-rolled rather than a general syntax highlighter - it
// only needs to understand the shape yaml.v3 actually emits for specDoc.
func specHTML(v interface{}) (template.HTML, error) {
	raw, err := yaml.Marshal(v)
	if err != nil {
		return "", err
	}

	var out bytes.Buffer
	lines := bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
	for i, line := range lines {
		if i > 0 {
			out.WriteByte('\n')
		}
		writeColoredLine(&out, string(line))
	}
	return template.HTML(out.String()), nil
}

func writeColoredLine(out *bytes.Buffer, line string) {
	if m := yamlLine.FindStringSubmatch(line); m != nil {
		indent, dash, key, value := m[1], m[2], m[3], m[4]
		out.WriteString(indent)
		if dash != "" {
			out.WriteString(`<span class="tok-punct">- </span>`)
		}
		out.WriteString(`<span class="tok-key">`)
		template.HTMLEscape(out, []byte(key))
		out.WriteString(`</span><span class="tok-punct">:</span>`)
		if value != "" {
			out.WriteByte(' ')
			out.WriteString(`<span class="tok-value">`)
			template.HTMLEscape(out, []byte(value))
			out.WriteString(`</span>`)
		}
		return
	}
	if m := yamlListItem.FindStringSubmatch(line); m != nil {
		indent, value := m[1], m[2]
		out.WriteString(indent)
		out.WriteString(`<span class="tok-punct">- </span>`)
		out.WriteString(`<span class="tok-value">`)
		template.HTMLEscape(out, []byte(value))
		out.WriteString(`</span>`)
		return
	}
	template.HTMLEscape(out, []byte(line))
}
