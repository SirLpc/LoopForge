import React from "react";
import { render, screen } from "@testing-library/react";
import { expect, test } from "vitest";
import ActivityTrace from "../src/ActivityTrace";
import { mergeEvents, restoreStream } from "../src/api";

const wait = { kind: "status", id: "workspace.pool_waiting", text: "正在等待预热工作区" };
const resume = { kind: "status", text: "正在调用模型…" };

test.each([false, true])("announces a warm wait after tools before model output, snapshot=%s", (snapshot) => {
  const activity = [{kind: "tool", id: "open", tool: "workbench_workspace_open"}, wait];
  const stream = snapshot ? restoreStream({ sequence: 2, activity }) : mergeEvents({}, activity.map((data, i) => ({ data, sequence: i + 1 })));
  const view = render(<ActivityTrace live workspaceWaiting steps={stream.activity} />);
  expect(screen.getByRole("status")).toHaveTextContent("等待预热工作区");
  // A reconnect between admission and the first new model event can replay the old tail.
  view.rerender(<ActivityTrace live workspaceWaiting={false} steps={stream.activity} />);
  expect(screen.queryByText(/等待预热工作区/)).not.toBeInTheDocument();
  const continued = mergeEvents(stream, [{data: resume, sequence: 3}]);
  view.rerender(<ActivityTrace live steps={continued.activity} />);
  expect(screen.queryByText(/等待预热工作区/)).not.toBeInTheDocument();
  view.rerender(<ActivityTrace interrupted steps={stream.activity} />);
  expect(screen.queryByRole("status")).not.toBeInTheDocument();
});
