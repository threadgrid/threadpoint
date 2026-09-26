"""Validate local links and section anchors in repository Markdown."""

import html
import re
import sys
import unicodedata
from pathlib import Path
from urllib.parse import unquote, urlsplit

root = Path(sys.argv[1])


def expand_indent(text, column=0):
    """Expand leading tabs at Markdown's four-column tab stops."""
    leading = re.match(r"^[ \t]*", text)[0]
    width = len((" " * column + leading).expandtabs(4)) - column
    return " " * width + text[len(leading) :]


def quote_content(text, column):
    marker = re.match(r"^ {0,3}>", text)
    if not marker:
        return None
    column += marker.end()
    content = expand_indent(text[marker.end() :], column)
    if content.startswith(" "):
        return content[1:], column + 1
    return content, column


def prose(path):
    """Remove code blocks relative to their list and blockquote containers."""
    text = re.sub(r"<!--.*?-->", "", path.read_text(encoding="utf-8"), flags=re.DOTALL)
    lines = []
    containers = []
    fence = None
    paragraph = False
    for raw_line in text.splitlines():
        content = expand_indent(raw_line)
        column = 0
        if not content.strip():
            # An unmarked blank line can remain inside a list, but ends a quote.
            for index, (kind, _) in enumerate(containers):
                if kind == "quote":
                    containers = containers[:index]
                    fence = None
                    break
            lines.append("")
            paragraph = False
            continue

        matched = 0
        for kind, width in containers:
            if kind == "quote":
                quoted = quote_content(content, column)
                if quoted is None:
                    break
                content, column = quoted
            elif content.startswith(" " * width):
                content = content[width:]
                column += width
            elif content.strip():
                break
            matched += 1
        if matched < len(containers):
            # A fence ends when its enclosing list item or blockquote ends,
            # even if the fence itself has no explicit closing marker.
            containers = containers[:matched]
            fence = None
            paragraph = False
            lines.append("")

        if fence:
            if re.fullmatch(
                r" {0,3}" + re.escape(fence[0]) + "{" + str(len(fence)) + r",}\s*",
                content,
            ):
                fence = None
            lines.append("")
            continue

        # Containers can alternate: a quote can contain a list, or vice versa.
        while True:
            quoted = quote_content(content, column)
            item = re.match(r"^ {0,3}(?:[-+*]|[0-9]{1,9}[.)])(?=[ \t]|$)", content)
            if quoted is not None:
                containers.append(("quote", 0))
                content, column = quoted
            elif item:
                remainder = expand_indent(content[item.end() :], column + item.end())
                padding = len(remainder) - len(remainder.lstrip(" "))
                # Five or more spaces after a marker introduce indented code;
                # only one of those spaces belongs to the list marker itself.
                padding = 1 if padding == 0 or padding > 4 else padding
                width = item.end() + padding
                containers.append(("list", width))
                content = remainder[padding:]
                column += width
            else:
                break
            paragraph = False
            lines.append("")

        marker = re.match(r"^ {0,3}(`{3,}|~{3,})", content)
        if marker:
            fence = marker[1]
            paragraph = False
            lines.append("")
        elif content.startswith("    ") and not paragraph:
            lines.append("")
        else:
            lines.append(content)
            # Indented code cannot interrupt an existing paragraph. Blank lines,
            # headings, and thematic breaks end that paragraph.
            paragraph = bool(content.strip()) and not re.match(
                r"^ {0,3}(?:#{1,6}(?:\s|$)|(?:[-*_]\s*){3,}$)", content
            )
    return "\n".join(lines)


# Inline destinations may be angle-bracketed, contain balanced parentheses, or
# have an optional title. Reference destinations use the same URL syntax.
destination = r"(?:<[^>\n]*>|(?:\\.|[^\s()\\]|\([^()\s]*\))+)"
inline_link = re.compile(
    r"\[[^\]\n]*\]\(\s*(" + destination + r")(?:\s+[\"'][^\n]*?[\"'])?\s*\)"
)
reference_link = re.compile(
    r"^ {0,3}\[[^\]\n]+\]:\s*(" + destination + r")", re.MULTILINE
)
# A soft line break can occur within a code span, but a blank line ends the
# paragraph and prevents stray backticks from hiding links in later paragraphs.
code_span = re.compile(r"(?<!`)(`+)(?!`)((?:[^\n]|\n(?![ \t]*\n))+?)(?<!`)\1(?!`)")


def links(text):
    text = code_span.sub("", text)
    for pattern in (inline_link, reference_link):
        for match in pattern.finditer(text):
            yield html.unescape(
                re.sub(
                    r"\\([!\"#$%&'()*+,\-./:;<=>?@\[\]^_`{|}~])",
                    r"\1",
                    match[1].strip("<>"),
                )
            )


def heading_slug(text):
    # Follow GitHub's section-link rules: rendered text, lowercase, spaces as
    # hyphens, punctuation removed. Duplicate IDs are disambiguated below.
    text = inline_link.sub(lambda match: match[0].split("](", 1)[0].lstrip("!["), text)
    text = re.sub(r"<[^>]*>", "", text)
    text = code_span.sub(lambda match: match[2], text)
    text = re.sub(r"(\*\*|~~|\*)(.+?)\1", r"\2", text)
    text = re.sub(r"(?<!\w)(__|_)(.+?)\1(?!\w)", r"\2", text)
    text = html.unescape(text).lower()
    return "".join(
        "-" if char == " " else char
        for char in text
        if char in " -_" or unicodedata.category(char)[0] in "LNM"
    )


def anchors(text):
    found = set()
    generated = set()
    lines = text.splitlines()
    for index, line in enumerate(lines):
        atx = re.match(r"^ {0,3}#{1,6}(?:\s+(.*?)\s*|\s*)$", line)
        setext = (
            index > 0
            and re.fullmatch(r" {0,3}(?:=+|-+)\s*", line)
            and lines[index - 1].strip()
        )
        if atx:
            heading = re.sub(r"\s+#+\s*$", "", atx[1] or "")
        elif setext:
            heading = lines[index - 1].strip()
        else:
            continue
        base = heading_slug(heading)
        candidate = base
        suffix = 0
        while candidate in generated:
            suffix += 1
            candidate = f"{base}-{suffix}"
        generated.add(candidate)
        found.add(candidate)
    # Honor explicit HTML anchors as well as generated heading IDs.
    for tag in re.finditer(r"<[A-Za-z][^>]*>", code_span.sub("", text)):
        for attr in re.finditer(
            r"\b(?:id|name)\s*=\s*([\"'])(.*?)\1", tag[0], re.IGNORECASE
        ):
            found.add(html.unescape(attr[2]))
    return found


files = {
    path
    for path in root.glob("*.md")
    if path.is_file() and not path.name.startswith(".env")
}
files.update(
    path
    for path in (root / "docs").rglob("*.md")
    if path.is_file()
    and "example-fixtures" not in path.relative_to(root / "docs").parts
    and not path.name.startswith(".env")
)
texts = {path: prose(path) for path in sorted(files)}
anchor_cache = {}
findings = set()
resolved_links = {}
for source, text in texts.items():
    resolved_links[source] = set()
    for target in links(text):
        url = urlsplit(target)
        if url.scheme or url.netloc:
            continue
        path = unquote(url.path)
        resolved = (
            (
                root / path.lstrip("/")
                if path.startswith("/")
                else source.parent / path
            ).resolve()
            if path
            else source
        )
        resolved_links[source].add(resolved)
        label = f"{source.relative_to(root)} -> {target}"
        if not resolved.exists():
            findings.add(f"broken link: {label}")
        elif url.fragment and resolved.is_file() and resolved.suffix.lower() == ".md":
            if resolved not in anchor_cache:
                anchor_cache[resolved] = anchors(
                    texts[resolved] if resolved in texts else prose(resolved)
                )
            if unquote(url.fragment) not in anchor_cache[resolved]:
                findings.add(f"broken fragment: {label}")

index = root / "docs/README.md"
if not index.is_file():
    findings.add("missing docs index: docs/README.md")
else:
    for doc in (root / "docs").glob("*.md"):
        if doc != index and doc.resolve() not in resolved_links.get(index, set()):
            findings.add(f"orphan page (not linked from docs/README.md): {doc.name}")

if findings:
    print("\n".join(sorted(findings)), file=sys.stderr)
    sys.exit(
        "docs link check failed: docs contain broken internal links, fragments, or unlinked pages"
    )
print("docs link check passed")
