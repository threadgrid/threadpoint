"""Exercise the docs link gate against isolated repository fixtures."""

import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


class DocsLinksTest(unittest.TestCase):
    def setUp(self):
        self.scratch = tempfile.TemporaryDirectory(
            prefix="threadpoint-docs-links-test-"
        )
        self.addCleanup(self.scratch.cleanup)
        self.root = Path(self.scratch.name)
        self.write("README.md", "# Project\n")
        self.write("docs/README.md", "# Documentation\n")
        self.write("scripts/check-docs-links.sh", "")
        for name in ("check-docs-links.sh", "check-docs-links.py"):
            shutil.copyfile(
                Path(__file__).with_name(name), self.root / "scripts" / name
            )

    def write(self, name, content):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8")

    def check(self):
        return subprocess.run(
            ["sh", str(self.root / "scripts/check-docs-links.sh")],
            cwd="/",
            capture_output=True,
            text=True,
            check=False,
        )

    def assert_passes(self):
        result = self.check()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def assert_fails(self, *messages):
        result = self.check()
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        for message in messages:
            self.assertIn(message, result.stderr)

    def test_root_markdown_missing_file(self):
        for name in ("CONTRIBUTING.md", "AGENTS.md", "SECURITY.md"):
            with self.subTest(name=name):
                self.write(name, "[policy](missing.md)\n")
                self.assert_fails(name, "missing.md", "broken link")
                (self.root / name).unlink()

    def test_contributing_heading_rename_breaks_fragment(self):
        self.write("AGENTS.md", "## Source Comments and Headers\n")
        self.write(
            "CONTRIBUTING.md",
            "[policy](AGENTS.md#source-comments-and-headers)\n",
        )
        self.assert_passes()
        self.write("AGENTS.md", "## Source Documentation\n")
        self.assert_fails(
            "CONTRIBUTING.md", "source-comments-and-headers", "broken fragment"
        )

    def test_same_file_fragment(self):
        self.write("README.md", "# Project\n[valid](#project)\n")
        self.assert_passes()
        self.write("README.md", "# Project\n[invalid](#missing)\n")
        self.assert_fails("README.md", "#missing", "broken fragment")

    def test_heading_styles_duplicates_and_encoded_paths(self):
        self.write(
            "Guide Notes.md",
            "# Hello, **World**!\n## Repeat\n## Repeat\n## Repeat-1\n"
            "Section Title\n-------------\n## Café\n",
        )
        self.write(
            "README.md",
            '[one](Guide%20Notes.md#hello-world "title")\n'
            "[two](<Guide Notes.md#repeat-1>)\n"
            "[collision](Guide%20Notes.md#repeat-1-1)\n"
            "[setext](Guide%20Notes.md#section-title)\n"
            "[unicode](Guide%20Notes.md#caf%C3%A9)\n",
        )
        self.assert_passes()

    def test_code_examples_are_not_links_or_headings(self):
        self.write(
            "README.md",
            "# Project\n```md\n# Fenced heading\n[example](missing.md)\n```\n"
            "~~~md\n[example](missing.md)\n~~~\n"
            "    [indented](missing.md)\n`[inline](missing.md)`\n"
            "<!-- [comment](missing.md) -->\n",
        )
        self.assert_passes()
        with (self.root / "README.md").open("a") as target:
            target.write("[invalid](#fenced-heading)\n")
        self.assert_fails("#fenced-heading", "broken fragment")

    def test_indented_list_continuation_links_are_checked(self):
        for body in (
            "- Item\n    [policy](missing.md)\n",
            "- Item\n      [policy](missing.md)\n",
            "- Item\n\n    [policy](missing.md)\n",
            "- Item\n\n\t[policy](missing.md)\n",
            "1. Item\n\n    [policy](missing.md)\n",
            "- Outer\n  - Inner\n\n      [policy](missing.md)\n",
            "- Outer\n    - Inner\n\n        [policy](missing.md)\n",
        ):
            with self.subTest(body=body):
                self.write("README.md", body)
                self.assert_fails("README.md", "missing.md", "broken link")

    def test_list_continuation_headings_are_fragment_targets(self):
        self.write("README.md", "- Item\n\n    ## Rules\n\n    [rules](#rules)\n")
        self.assert_passes()
        self.write("README.md", "- Item\n\n    ## Renamed\n\n    [rules](#rules)\n")
        self.assert_fails("#rules", "broken fragment")

    def test_indented_and_fenced_code_inside_lists_stays_excluded(self):
        for body in (
            "- Item\n\n      [example](missing.md)\n",
            "- Item\n\nParagraph outside the list.\n\n    [example](missing.md)\n",
            "- Item\n\n  ```md\n  [example](missing.md)\n  ```\n",
            "- Outer\n  - Inner\n\n    ~~~md\n    [example](missing.md)\n    ~~~\n",
        ):
            with self.subTest(body=body):
                self.write("README.md", body)
                self.assert_passes()

    def test_code_inside_blockquotes_stays_excluded(self):
        for body in (
            ">     [example](missing.md)\n",
            "> \t  [example](missing.md)\n",
            "> >     [example](missing.md)\n",
            "> ```md\n> [example](missing.md)\n> ```\n",
            "```md\n> [example](missing.md)\n```\n",
            "    > [example](missing.md)\n",
            "> - Item\n>\n>       [example](missing.md)\n",
            "- Item\n\n  >     [example](missing.md)\n",
            "- >     [example](missing.md)\n",
        ):
            with self.subTest(body=body):
                self.write("README.md", body)
                self.assert_passes()

    def test_blockquote_prose_links_are_checked(self):
        for body in (
            "> [policy](missing.md)\n",
            "> \t[policy](missing.md)\n",
            "> > [policy](missing.md)\n",
            "> - Item\n>\n>     [policy](missing.md)\n",
            "- Item\n\n  > [policy](missing.md)\n",
        ):
            with self.subTest(body=body):
                self.write("README.md", body)
                self.assert_fails("missing.md", "broken link")

    def test_leaving_blockquote_closes_its_unfinished_fence(self):
        self.write(
            "README.md",
            "> ```md\n> [example](ignored.md)\n[policy](missing.md)\n",
        )
        self.assert_fails("missing.md", "broken link")
        self.assertNotIn("ignored.md", self.check().stderr)

    def test_blockquote_headings_are_fragment_targets(self):
        self.write("README.md", "> ## Quoted Rules\n>\n> [rules](#quoted-rules)\n")
        self.assert_passes()
        self.write("README.md", "> ## Renamed\n>\n> [rules](#quoted-rules)\n")
        self.assert_fails("#quoted-rules", "broken fragment")

    def test_code_spans_cannot_cross_blank_lines(self):
        for blank in ("\n\n", "\n  \n", "\n\t\n"):
            for delimiter in ("`", "``"):
                with self.subTest(blank=blank, delimiter=delimiter):
                    self.write(
                        "README.md",
                        f"Stray {delimiter}opener{blank}"
                        f"[policy](missing.md){blank}closer{delimiter}\n",
                    )
                    self.assert_fails("missing.md", "broken link")

    def test_multiline_code_span_within_paragraph_stays_excluded(self):
        self.write("README.md", "Example `starts here\n[example](missing.md)` ends.\n")
        self.assert_passes()

    def test_external_links_and_non_markdown_fragments(self):
        self.write("icon.svg", '<svg id="mark"></svg>')
        self.write(
            "README.md",
            "[web](https://example.invalid/missing#heading)\n"
            "[mail](mailto:hello@example.invalid)\n"
            "[network](//example.invalid/missing)\n"
            "[asset](icon.svg#mark)\n",
        )
        self.assert_passes()

    def test_orphan_pages_and_fixture_exclusion(self):
        self.write("docs/example-fixtures/example/README.md", "[bad](missing.md)\n")
        self.write("docs/guide.md", "# Guide\n")
        self.assert_fails("orphan page", "guide.md")
        self.write("docs/README.md", "# Docs\n[guide](guide.md#guide)\n")
        self.assert_passes()

    def test_reference_links_and_explicit_anchors(self):
        self.write("AGENTS.md", '<a id="custom-anchor"></a>\n')
        self.write(
            "CONTRIBUTING.md", "[policy][rules]\n\n[rules]: AGENTS.md#custom-anchor\n"
        )
        self.assert_passes()
        self.write("AGENTS.md", '<a id="renamed-anchor"></a>\n')
        self.assert_fails("CONTRIBUTING.md", "custom-anchor", "broken fragment")

    def test_heading_inline_code_and_underscores(self):
        self.write(
            "README.md",
            "# Use `go test` and foo_bar_baz\n"
            "[heading](#use-go-test-and-foo_bar_baz)\n",
        )
        self.assert_passes()

    def test_missing_docs_index(self):
        (self.root / "docs/README.md").unlink()
        self.assert_fails("missing docs index")

    def test_nested_docs_and_repository_relative_links(self):
        self.write("AGENTS.md", "## Rules\n")
        self.write("docs/nested/guide.md", "[policy](/AGENTS.md#rules)\n")
        self.assert_passes()
        self.write("docs/nested/guide.md", "[policy](../../AGENTS.md#missing)\n")
        self.assert_fails("docs/nested/guide.md", "broken fragment")


if __name__ == "__main__":
    unittest.main()
