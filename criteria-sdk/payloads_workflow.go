package criteria

import pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

// SubworkflowGraph is one compiled subworkflow layer of a run's workflow
// (CRI-257): the subworkflow name from the parent module's declaration, the
// module path the parent declared, and the compiled module source.
type SubworkflowGraph = pb.SubworkflowGraph

// WorkflowGraphs is emitted by the agent after it compiles the run's
// workflow (CRI-257). It carries the compiled subworkflow layers the
// top-level module references so consumers can render subworkflow graphs
// without file access.
type WorkflowGraphs = pb.WorkflowGraphs
