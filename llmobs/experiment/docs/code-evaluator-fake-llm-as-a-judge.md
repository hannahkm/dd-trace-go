# Fake LLM-as-a-Judge Evaluator (Categorical Label Pattern)

## What this evaluator does

This evaluator demonstrates a **categorical judgment** pattern by always returning the same quality label.

Example label: `excellent`.

It exists to illustrate how judge-style outcomes can be represented in experiment metrics.

## Inputs it reads

Conceptually, judge evaluators look at:

- the dataset record context,
- the task output,
- (optionally) rubric or policy criteria.

In this simplified pattern, the returned label is constant and does not vary by input.

## Evaluation logic (step-by-step)

1. Receive record/output context.
2. Assign a qualitative label.
3. Return that label as the evaluation value.

## Output type and downstream metric mapping

- Evaluator return type: **string label**.
- Experiment metric type generated from this value: **`categorical`**.

Typical dashboards then slice counts/ratios by label.

## Why this evaluator exists

It demonstrates the integration contract for LLM-as-a-judge style evaluators without requiring a real model call.

## How to use this pattern in production

Replace the constant label with model- or rubric-driven labeling, for example:

- `excellent` / `good` / `fair` / `poor`,
- `safe` / `unsafe`,
- `grounded` / `hallucinated`.

For reliability, define clear rubric criteria and keep labels stable over time.
