# Similarity Evaluator (Heuristic Score Pattern)

## What this evaluator does

This evaluator demonstrates a **minimal numeric scoring pattern** for experiments.

It emits one of two scores:

- `1.0` if output exactly matches expected output,
- `0.5` if it does not.

The goal is to show the score-evaluator contract, not provide a production-quality similarity method.

## Inputs it reads

For each record evaluation call, it consumes:

- **Task output**,
- **Expected output** from the dataset record.

## Evaluation logic (step-by-step)

1. Compare produced and expected values.
2. Return full score (`1.0`) for exact match.
3. Return fallback partial score (`0.5`) otherwise.

## Output type and downstream metric mapping

- Evaluator return type: **floating-point score**.
- Experiment metric type generated from this value: **`score`**.

This makes the result aggregatable as an average quality signal across records.

## Why this evaluator exists

It is intentionally simple so that test/example flows can validate score handling without depending on heavy NLP/ML logic.

## When to use it

Use this pattern as a scaffold when building your own scoring evaluator and you want to wire experiment metrics first.

## How to evolve it for production

Replace the fixed fallback value with domain-aware scoring, such as:

- edit-distance normalization,
- token overlap,
- embedding cosine similarity,
- rubric-based judge scoring.
