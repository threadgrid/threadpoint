// SPDX-License-Identifier: Apache-2.0

// Package skill parses agent skill definition files and exposes their metadata.
package skill

import (
	"bufio"
	"errors"
	"strings"
)

// Metadata is the portable frontmatter extracted from a skill definition file.
type Metadata struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ParseFrontmatter parses SKILL.md-style YAML frontmatter for the portable skill
// name and description fields.
func ParseFrontmatter(markdown []byte) (Metadata, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(markdown)))
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return Metadata{}, errors.New("missing frontmatter opening marker")
	}

	meta := Metadata{}
	var descriptionLines []blockScalarLine
	inDescriptionBlock := false
	descriptionFolded := false
	descriptionParentIndent := 0
	descriptionContentIndent := -1
	finishDescription := func() {
		meta.Description = joinDescription(descriptionLines, descriptionFolded)
		inDescriptionBlock = false
	}
	for scanner.Scan() {
		raw := scanner.Text()
		line := strings.TrimSpace(raw)
		if inDescriptionBlock {
			if line != "" && leadingIndent(raw) <= descriptionParentIndent {
				finishDescription()
			} else {
				if line == "" {
					indent := leadingIndent(raw)
					if descriptionContentIndent >= 0 && indent > descriptionContentIndent {
						descriptionLines = append(descriptionLines, blockScalarLine{
							text:         raw[descriptionContentIndent:],
							moreIndented: true,
						})
						continue
					}
					descriptionLines = append(descriptionLines, blockScalarLine{blank: true})
					continue
				}
				indent := leadingIndent(raw)
				if descriptionContentIndent < 0 {
					descriptionContentIndent = indent
				}
				if indent < descriptionContentIndent {
					return Metadata{}, errors.New("description block indentation is invalid")
				}
				value := raw[descriptionContentIndent:]
				descriptionLines = append(descriptionLines, blockScalarLine{
					text:         value,
					moreIndented: leadingIndent(value) > 0,
				})
				continue
			}
		}
		if line == "---" {
			if meta.Name == "" {
				return Metadata{}, errors.New("skill name is required")
			}
			if strings.TrimSpace(meta.Description) == "" {
				return Metadata{}, errors.New("skill description is required")
			}
			return meta, nil
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), "\"'")
		switch strings.TrimSpace(key) {
		case "name":
			meta.Name = value
		case "description":
			if header, ok := blockScalarHeader(value); ok {
				inDescriptionBlock = true
				descriptionFolded = header.folded
				descriptionParentIndent = leadingIndent(raw)
				descriptionContentIndent = -1
				if header.indent > 0 {
					descriptionContentIndent = descriptionParentIndent + header.indent
				}
				descriptionLines = nil
			} else {
				meta.Description = value
			}
		}
	}
	if inDescriptionBlock {
		finishDescription()
	}
	if err := scanner.Err(); err != nil {
		return Metadata{}, err
	}
	return Metadata{}, errors.New("missing frontmatter closing marker")
}

// blockScalarHeader recognizes YAML literal and folded block-scalar headers,
// including valid chomping, indentation, and comment suffixes. The parser only
// needs the style here; chomping affects trailing newlines, which the portable
// metadata contract trims.
type blockScalarHeaderInfo struct {
	folded bool
	indent int
}

type blockScalarLine struct {
	text         string
	blank        bool
	moreIndented bool
}

func blockScalarHeader(value string) (header blockScalarHeaderInfo, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" || (value[0] != '|' && value[0] != '>') {
		return blockScalarHeaderInfo{}, false
	}
	header.folded = value[0] == '>'
	rest := strings.TrimSpace(value[1:])
	seenChomp := false
	seenIndent := false
	for len(rest) > 0 {
		switch rest[0] {
		case '+', '-':
			if seenChomp {
				return blockScalarHeaderInfo{}, false
			}
			seenChomp = true
			rest = rest[1:]
		case '1', '2', '3', '4', '5', '6', '7', '8', '9':
			if seenIndent {
				return blockScalarHeaderInfo{}, false
			}
			seenIndent = true
			header.indent = int(rest[0] - '0')
			rest = rest[1:]
		case '#':
			return header, true
		case ' ', '\t':
			rest = strings.TrimLeft(rest, " \t")
			if rest == "" || rest[0] == '#' {
				return header, true
			}
			return blockScalarHeaderInfo{}, false
		default:
			return blockScalarHeaderInfo{}, false
		}
	}
	return header, true
}

func leadingIndent(value string) int {
	for index, character := range value {
		if character != ' ' && character != '\t' {
			return index
		}
	}
	return len(value)
}

func joinDescription(lines []blockScalarLine, folded bool) string {
	if !folded {
		values := make([]string, len(lines))
		for index, line := range lines {
			values[index] = line.text
		}
		return strings.TrimRight(strings.Join(values, "\n"), "\n")
	}
	var value strings.Builder
	previousMoreIndented := false
	previous := false
	blankLines := 0
	for _, line := range lines {
		if line.blank {
			blankLines++
			continue
		}
		if previous {
			switch {
			case blankLines > 0:
				newlines := blankLines
				if previousMoreIndented || line.moreIndented {
					newlines++
				}
				value.WriteString(strings.Repeat("\n", newlines))
			case previousMoreIndented || line.moreIndented:
				value.WriteByte('\n')
			default:
				value.WriteByte(' ')
			}
		} else if blankLines > 0 {
			value.WriteString(strings.Repeat("\n", blankLines))
		}
		value.WriteString(line.text)
		previous = true
		previousMoreIndented = line.moreIndented
		blankLines = 0
	}
	if blankLines > 0 {
		value.WriteString(strings.Repeat("\n", blankLines))
	}
	return strings.TrimRight(value.String(), "\n")
}
