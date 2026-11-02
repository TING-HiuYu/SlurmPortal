"""check_code.py: every fenced block (except mermaid) in each Chinese page equals the English one."""
import re, sys, pathlib
root = pathlib.Path(__file__).resolve().parents[2]
zh_root = root / "site/i18n/zh-Hans/docusaurus-plugin-content-docs/current"
def blocks(text):
    return [(lang, body) for lang, body in re.findall(r"^```([^\n]*)\n(.*?)^```", text, re.S | re.M) if not lang.startswith("mermaid")]
bad = 0
for zh in sorted(zh_root.rglob("*.md")):
    en = root / "docs" / zh.relative_to(zh_root)
    if not en.exists():
        print("no English page for", zh); bad += 1; continue
    a, b = blocks(en.read_text()), blocks(zh.read_text())
    if a != b:
        bad += 1
        print(f"{zh.relative_to(zh_root)}: {len(b)} blocks vs {len(a)}")
        for i, (x, y) in enumerate(zip(a, b)):
            if x != y:
                print("  first difference in block", i + 1, repr(x[1][:80]), "!=", repr(y[1][:80])); break
print("pages checked:", len(list(zh_root.rglob('*.md'))), "mismatched:", bad)
sys.exit(1 if bad else 0)
