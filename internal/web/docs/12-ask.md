# Ask (your knowledge, in plain language)

The **Ask** tab answers a question in plain English using only your team's own notes
and code, with citations, never invented. It is the conversational front door to the
same knowledge an agent reads over MCP.

## How to use it

Type a question ("how do we authenticate Mollie webhooks?", "what is our SSRF
policy?") and press Ask. Mesh retrieves the relevant notes (scoped to what you are
allowed to read) plus the relevant code symbols, hands them to your LLM as the ONLY
allowed context, and returns the answer with numbered sources. Click a note citation to
open it.

On the command line:

```
mesh ask "why did we pick SQLite over Postgres?"
```

## Grounded, not guessing

The model is told to answer only from the provided context and to say "the vault has
nothing on that" when it does not, instead of making something up. Every claim carries
a citation back to the note or `file:line` it came from, so you can verify the answer
rather than trust it.

The context is the actual text of the top-ranked notes, not the short search excerpt,
so the answer is grounded in what the notes say rather than in their titles. That text
is packed to a token budget (3000 by default, `--budget` on the CLI, `budget` in the
API request), and a source that does not fit is dropped rather than cited: the numbered
sources under an answer are exactly what the model was given.

## Bring your own model

Ask is BYOAI, like the rest of Mesh's optional AI. It uses whatever you configure via
`MESH_CURATOR_*` (by default your already-authenticated coding-agent CLI, so there is
no separate API key to hold; an Anthropic key or a local OpenAI-compatible endpoint
also work). On a host with no model configured (for example a server with no CLI
logged in), the Ask box degrades gracefully with a clear message and the rest of the
app is unaffected.

## When to use Ask vs Search vs an agent

- **Search** shows you the exact ranked notes an agent would get, with no LLM in the
  loop. Use it to see what is there.
- **Ask** synthesizes an answer across several notes and code, with citations. Use it
  when you want the answer, not the list.
- **An agent over MCP** does both as part of doing real work, and writes back what it
  learns. That is the flywheel.
