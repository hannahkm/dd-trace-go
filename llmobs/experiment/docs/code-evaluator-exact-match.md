# Exact Match Evaluator

## What this evaluator does

The Exact Match evaluator answers one question for each dataset record:

**“Did the task output exactly equal the expected output?”**

It emits a binary pass/fail result.

## Inputs it reads

For each record evaluation call, it consumes:

- **Task output**: the value produced by the experiment task for that record.
- **Expected output**: the gold/reference answer stored in the dataset record.

## Evaluation logic (step-by-step)

1. Receive the task output and expected output.
2. Perform strict equality comparison between the two values.
3. Return:
   - `true` when values are exactly equal,
   - `false` when they differ.

## Output type and downstream metric mapping

- Evaluator return type: **boolean** (`true`/`false`).
- Experiment metric type generated from this value: **`boolean`**.

In dashboards, this behaves as a pass-rate style signal (for example, percent of records with `true`).

## Error behavior

This evaluator typically does not produce its own error unless the equality comparison cannot be performed for the concrete runtime types.

If an error does occur, experiment behavior depends on run configuration:

- default: error is recorded and execution continues,
- abort mode (`WithAbortOnError(true)`): run stops on evaluator failure.

## When to use it

Use Exact Match when outputs are deterministic and canonicalized, such as:

- classification labels,
- IDs,
- normalized short answers,
- exact expected strings.

## When it is not sufficient

Avoid relying only on Exact Match when acceptable answers can vary in wording, format, ordering, or punctuation. In those cases pair it with a fuzzy score evaluator.
