# Overlap Evaluator (Jaccard Similarity on Character Sets)

## What this evaluator does

The Overlap evaluator measures **how much the produced answer and expected answer share in common** using set similarity.

It returns a numeric score in the range **0.0 to 1.0**.

## Inputs it reads

For each record evaluation call, it consumes:

- **Task output** (expected to be text),
- **Expected output** (expected to be text).

If either value is not textual, the evaluator reports an error.

## Evaluation logic (step-by-step)

1. Convert both answers into sets of unique characters (runes).
2. Compute:
   - **intersection size**: number of characters present in both sets,
   - **union size**: number of unique characters present in either set.
3. Return Jaccard similarity:
   - `intersection / union` when union is non-zero,
   - `1.0` when both sets are empty.

Interpretation:

- `1.0` means identical character-set coverage,
- `0.0` means no shared characters,
- intermediate values represent partial overlap.

## Output type and downstream metric mapping

- Evaluator return type: **floating-point score**.
- Experiment metric type generated from this value: **`score`**.

## Error behavior

The evaluator can fail if output shapes are incompatible with text comparison (for example, non-string values). Error handling then follows experiment run settings (continue vs abort).

## Characteristics to keep in mind

- Set-based: repeated characters do not increase score.
- Character-level: it does not understand words, syntax, or semantics.
- Case-sensitive unless pre-normalized before evaluation.
- Fast and deterministic.

## When to use it

Use this as a lightweight fuzzy signal when exact equality is too strict and you want a cheap, deterministic similarity score.

## When it is not sufficient

For semantic correctness (meaning-level similarity), use stronger evaluators (embedding similarity, rubric-based judge, or domain-specific scoring).
