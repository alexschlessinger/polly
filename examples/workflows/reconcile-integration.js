// Input: {"id":"candidate-or-apply-id"}
// Run this separately after inspecting an interrupted workflow's saved report.
polly.defineWorkflow({
  name: "reconcile-integration",
  inputSchema: polly.schema.object({id: polly.schema.string()}),
  async run({id}) {
    await polly.integration.reconcile(id);
    const integration = await polly.integration.read(id);
    const receipt = integration.receipt;
    // Reconciliation observes the recorded plan's before/after states. It
    // never applies a patch. Inspect not_applied or recovery_required before
    // choosing an explicit retry or repair in a separate action.
    return {receipt, integration};
  },
});
