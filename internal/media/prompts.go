package media

import (
	"fmt"
	"strings"
)

// This file holds the built-in document-vision prompts behind the
// `--transcribe` / `--translate` flags. They are inlined in Go (rather
// than living in ~/.qai/profiles/*.yaml) so the feature works on a fresh
// install with no profile files present. `--system` still wins over
// these — pass it to override the system instruction entirely.
//
// Intent mirrors the exact-handwriting-extractor worker: a photographed
// page of handwritten or printed text becomes clean, faithful Markdown.
// The cardinal rule is verbatim fidelity — never summarise, correct,
// complete, or invent. The translate layer adds a second pass that
// renders the same content into a target language while preserving
// structure, names, numbers, code, and math.

// transcribeSystem is the system instruction for both transcribe and
// translate modes. It frames the model as a faithful transcription
// engine, not an assistant — no chatter, no summarising, no "helpful"
// completion of half-finished sentences.
const transcribeSystem = "You are a meticulous document transcription engine. " +
	"You convert photographs of handwritten or printed pages into clean, faithful Markdown. " +
	"You transcribe exactly what is on the page — you never summarise, paraphrase, correct, " +
	"complete, modernise, or invent content. You preserve the author's own wording, spelling, " +
	"and punctuation. You output only the requested Markdown, with no preamble or commentary."

// transcribeRules is the shared body of structural rules used by the
// faithful-transcription pass. Kept separate so the translate prompt can
// reuse the exact same fidelity contract for its first pass.
const transcribeRules = "Rules:\n" +
	"- Reproduce every word verbatim — same wording, spelling, and punctuation. " +
	"Do not correct, complete, modernise, or summarise anything.\n" +
	"- Preserve structure: render headings as Markdown headings, lists as Markdown lists, " +
	"and any tabular data as a Markdown table.\n" +
	"- Preserve visible emphasis only when unambiguous (e.g. underline or boxed text → **bold**).\n" +
	"- For any word you cannot read with confidence, write your best guess in [brackets]; " +
	"if a word is wholly illegible, write [illegible].\n" +
	"- Do not add titles, commentary, or explanations of your own. " +
	"Output only the transcribed Markdown — nothing before or after it."

// buildDocPrompt returns the user-turn prompt for the requested mode.
// At least one of transcribe/translate is true (the caller guarantees
// it). target is the translation target language name (e.g. "English");
// ignored when translate is false.
//
//	transcribe only            → faithful verbatim Markdown
//	translate only             → faithful pass, then output ONLY the translation
//	transcribe AND translate   → output the verbatim transcription, then a
//	                             "## Translation (<target>)" section beneath it
func buildDocPrompt(transcribe, translate bool, target string) string {
	if target == "" {
		target = "English"
	}

	switch {
	case transcribe && translate:
		return strings.Join([]string{
			"Transcribe this page into Markdown exactly as written, then translate it.",
			transcribeRules,
			"",
			fmt.Sprintf(
				"Then, below the verbatim transcription, add a section headed "+
					"\"## Translation (%s)\" containing the full transcription translated into %s. "+
					"Apply the same structure rules to the translation. Leave names, numbers, code, "+
					"and mathematical expressions unchanged.", target, target),
			"",
			"Output exactly two parts: the verbatim Markdown transcription first, then the " +
				"\"## Translation\" section. Nothing else.",
		}, "\n")

	case translate:
		return strings.Join([]string{
			fmt.Sprintf("Transcribe this page faithfully, then translate it into %s.", target),
			transcribeRules,
			"",
			fmt.Sprintf(
				"Output ONLY the %s translation as Markdown, applying the same structure rules. "+
					"Do not include the original-language text. Leave names, numbers, code, and "+
					"mathematical expressions unchanged.", target),
		}, "\n")

	default: // transcribe only
		return "Transcribe this page into Markdown, exactly as written.\n\n" + transcribeRules
	}
}
