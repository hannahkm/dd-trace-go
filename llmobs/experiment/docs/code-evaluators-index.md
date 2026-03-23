# LLMObs Experiment SDK: Code Evaluator Documentation

This directory explains the evaluator patterns currently used by the Go LLMObs experiment SDK examples/tests.

The pages are written to be standalone references so readers can understand evaluator behavior without opening source files.

## Evaluator pages

1. [Exact Match evaluator](./code-evaluator-exact-match.md)
2. [Overlap evaluator (Jaccard on character sets)](./code-evaluator-overlap.md)
3. [Similarity evaluator (heuristic score pattern)](./code-evaluator-similarity.md)
4. [Fake LLM-as-a-Judge evaluator (categorical label pattern)](./code-evaluator-fake-llm-as-a-judge.md)

## How experiment evaluations run (end-to-end)

An experiment run processes evaluations in five phases:

1. **Task execution phase**
   - Each dataset record is executed by the configured task.
   - The run captures output, timestamps, and tracing metadata per record.

2. **Per-record evaluator phase**
   - Each configured evaluator runs against each record output.
   - Each evaluator produces an `Evaluation` containing name, value, and optional error.

3. **Summary evaluator phase (optional)**
   - Aggregate evaluators can run after per-record processing.
   - These operate over the full set of record results.

4. **Metric normalization phase**
   - Evaluation values are mapped to Datadog experiment metric types:
     - booleans → `boolean`,
     - numbers → `score`,
     - other values (for example strings) → `categorical`.

5. **Publish phase**
   - Normalized evaluation metric events are sent to the backend for analysis and visualization.

## Error-handling model

Evaluator failures are captured on the corresponding evaluation item.

- In default mode, runs continue after evaluator errors.
- In abort mode (`WithAbortOnError(true)`), evaluator errors stop the run.

## Design guidance for custom evaluators

When creating custom evaluators, choose return types intentionally because type controls metric semantics:

- return **bool** for pass/fail,
- return **number** for quality scores,
- return **string/enum** for category labels.

This keeps downstream experiment analytics interpretable and consistent.
