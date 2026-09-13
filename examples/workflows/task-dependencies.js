// Input: {"question":"How does the cache work?","feedback":"Verify the failure paths independently."}
polly.defineWorkflow({
  name: "task-dependencies",
  inputSchema: polly.schema.object({question: polly.schema.string(), feedback: polly.schema.string()}),
  async run({question, feedback}) {
    const evidence = await polly.tasks.create({description: question});
    const assessment = await polly.tasks.create({
      description: "Assess the evidence and its limitations", review: true,
      dependencies: [evidence.id],
    });
    const research = await polly.agent({
      taskID: evidence.id, label: "Gather evidence", task: question, readOnly: true,
    });
    // Durable delivery completes the dependency before this launch.
    const first = await polly.agent({
      taskID: assessment.id, label: "Assess evidence", task: assessment.description,
      input: research.value, readOnly: true,
    });
    let task = await polly.tasks.read(first.task);
    await polly.tasks.review({task: task.id, revision: task.revision, accept: false, feedback});
    task = await polly.tasks.read(task.id);
    // Clear the inactive owner; the scheduler assigns the replacement agent.
    // An active owner must finish or its owning workflow must be canceled first.
    task = await polly.tasks.update({
      task: task.id, revision: task.revision, owner: "", dependencies: [evidence.id],
    });
    const replacement = await polly.agent({
      taskID: task.id, label: "Verify assessment", task: feedback,
      input: {evidence: research.value, assessment: first.value}, readOnly: true,
    });
    task = await polly.tasks.read(replacement.task);
    await polly.tasks.review({task: task.id, revision: task.revision, accept: true});
    return {research, first, replacement, task: await polly.tasks.read(task.id)};
  },
});
