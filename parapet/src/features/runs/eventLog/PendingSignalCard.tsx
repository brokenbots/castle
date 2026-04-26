interface PendingSignalCardProps {
  signal: string;
  runId: string;
}

export function PendingSignalCard({ signal, runId }: PendingSignalCardProps) {
  const curlExample = `curl -X POST http://localhost:8080/overlord.v1.OverseerService/Resume \\
  -H "Content-Type: application/json" \\
  -d '{"run_id":"${runId}","signal":"${signal}"}'`;

  const handleCopy = () => {
    navigator.clipboard.writeText(curlExample);
  };

  return (
    <div className="bg-slate-800 rounded px-4 py-3 border border-amber-700/50">
      <p className="text-sm text-slate-300 mb-2">
        <span className="text-amber-400 font-semibold">Waiting for signal</span>: <span className="font-mono">{signal}</span>
      </p>
      <details className="text-xs text-slate-400">
        <summary className="cursor-pointer hover:text-slate-300">Resume via curl</summary>
        <div className="mt-2 relative">
          <pre className="bg-slate-900 rounded p-2 overflow-x-auto text-xs">{curlExample}</pre>
          <button
            onClick={handleCopy}
            className="absolute top-2 right-2 px-2 py-1 bg-slate-700 hover:bg-slate-600 rounded text-xs"
          >
            Copy
          </button>
        </div>
      </details>
    </div>
  );
}
