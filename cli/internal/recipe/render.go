package recipe

import (
	"bytes"
	"strings"

	"go.yaml.in/yaml/v3"
)

// YAML renders the draft as the environment block of casebox.yml, with its notes as comments
// above it, so a person sees where each field came from before confirming it.
func (d Draft) YAML() (string, error) {
	var body yaml.Node
	if err := body.Encode(d.Recipe); err != nil {
		return "", err
	}
	key := &yaml.Node{Kind: yaml.ScalarNode, Value: "environment"}
	var comment []string
	comment = append(comment, "Drafted by casebox; check it with casebox env check, then confirm it with casebox env confirm.")
	for _, n := range d.Notes {
		comment = append(comment, n.Field+": "+n.Text)
	}
	key.HeadComment = strings.Join(comment, "\n")
	doc := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{key, &body}}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return "", err
	}
	return buf.String(), enc.Close()
}
