# Contract (the edge)

HTTP, the published contract between jellyscale and Octave, and the one writer of refusals.

## Words

- **Capability:** a group of operations the contract declares.
- **Reason:** plain words for a refusal or a conversion, from one list. jellyscale owns what each means; Octave owns the words a viewer reads.
- **Action:** what an app may do next, such as starting again.

## Must not

Hold domain logic or SQL.

## Rules

1. The contract is the source of truth for what the server offers. An operation, an answer or a refusal is in the contract, with an example, before the server serves it.
2. Every refusal carries a reason from the one list, and each reason has one owning domain. A refusal is answered as a refusal, never inside a success.
3. Every operation declares who may call it. Nothing is open unless the contract says so and says why. (Which pictures are open is an open question: PRODUCT.md.)
4. Additions are compatible: an app ignores fields, reasons and actions it doesn't know. A change that would break a supported Octave release is a new version, served beside the old.
5. Names and shapes are jellyscale's own, chosen for Octave. None is kept because another server used it.
6. Every change to the contract reaches Octave as an issue saying what changed and what each platform must do.
