// Input: {"question":"How does the cache work?","feedback":"Verify the failure paths independently."}
const {obj, str} = polly.schema;
polly.workflow("task-dependencies", obj({question: str(), feedback: str()}), async ({question, feedback}) => {
  const evidence = await polly.tasks.create({description: question});
  const assessment = await polly.tasks.create({
    description: "Assess the evidence and its limitations", review: true,
    dependencies: [evidence.id],
  });
  const research = await polly.research("Gather evidence", question, {taskID: evidence.id});
  // Durable delivery completes the dependency before this launch.
  const first = await polly.research("Assess evidence", assessment.description, {
    taskID: assessment.id, input: research.value,
  });
  let {id, revision} = await polly.tasks.get(first.task);
  await polly.tasks.review({task: id, revision, accept: false, feedback});
  let task = await polly.tasks.get(id);
  // Clear the inactive owner; the scheduler assigns the replacement agent.
  // An active owner must finish or its owning workflow must be canceled first.
  task = await polly.tasks.update({
    task: task.id, revision: task.revision, owner: "", dependencies: [evidence.id],
  });
  const replacement = await polly.research("Verify assessment", feedback, {
    taskID: task.id, input: {evidence: research.value, assessment: first.value},
  });
  task = await polly.tasks.get(replacement.task);
  await polly.tasks.review({task: task.id, revision: task.revision, accept: true});
  return {research, first, replacement, task: await polly.tasks.get(task.id)};
});
