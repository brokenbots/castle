import { Link } from 'react-router-dom';
import { useListRunsQuery } from '../../api/castleApi';

const statusColor: Record<string, string> = {
  running: 'text-amber-400',
  succeeded: 'text-emerald-400',
  failed: 'text-rose-400',
  pending: 'text-slate-400',
  cancelled: 'text-slate-500',
};

export function RunListPage() {
  const { data, isLoading, error } = useListRunsQuery();
  if (isLoading) return <p>Loading runs…</p>;
  if (error) return <p className="text-rose-400">Failed to load runs.</p>;
  return (
    <div>
      <h2 className="text-2xl font-semibold mb-4">Runs</h2>
      <table className="w-full text-sm">
        <thead className="text-left text-slate-400 border-b border-slate-800">
          <tr>
            <th className="py-2 pr-4">ID</th>
            <th className="py-2 pr-4">Workflow</th>
            <th className="py-2 pr-4">Status</th>
            <th className="py-2 pr-4">Started</th>
          </tr>
        </thead>
        <tbody>
          {(data ?? []).map((r) => (
            <tr key={r.runId} className="border-b border-slate-900 hover:bg-slate-900/40">
              <td className="py-2 pr-4 font-mono text-xs">
                <Link className="text-sky-400 hover:underline" to={`/runs/${r.runId}`}>
                  {r.runId.slice(0, 8)}
                </Link>
              </td>
              <td className="py-2 pr-4">{r.workflowName}</td>
              <td className={`py-2 pr-4 ${statusColor[r.status] ?? ''}`}>{r.status}</td>
              <td className="py-2 pr-4 text-slate-400">
                {r.createdAt ? new Date(r.createdAt).toLocaleString() : ''}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
