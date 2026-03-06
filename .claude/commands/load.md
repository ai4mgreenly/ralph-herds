Load one or more skills from `.claude/library/` into context.

**Usage:**
- `/load NAME` - Load NAME skill
- `/load NAME1 NAME2 ...` - Load multiple skills
- `/load path/to/skill` - Load from subfolder

**Examples:**
- `/load jj` - Load `.claude/library/jj/SKILL.md`
- `/load jj style` - Load multiple skills

**Arguments:** Space-separated skill names/paths (no .md extension)

---

{{#each (split args " ")}}
Read `.claude/library/{{this}}/SKILL.md`
{{/each}}

Then wait for instructions.
