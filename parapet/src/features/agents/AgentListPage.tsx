import { Link } from 'react-router-dom';
import { useListAgentsQuery } from '../../api/castleApi';
import { PageHeader } from '../../components/PageHeader';

export function AgentListPage() {
  const { data, isLoading, error } = useListAgentsQuery();
  if (isLoading) return <p>Loading…</p>;
  if (error) return <p className="text-danger">Failed to load.</p>;
  return (
    <div>
      <PageHeader title="Agents" />
      <table className="w-full text-sm">
        <thead className="text-left text-slate-400 border-b border-slate-800">
          <tr>
            <th className="py-2 pr-4">Name</th>
            <th className="py-2 pr-4">Hostname</th>
            <th className="py-2 pr-4">Status</th>
            <th className="py-2 pr-4">Last seen</th>
          </tr>
        </thead>
        <tbody>
          {(data ?? []).map((a) => (
            <tr key={a.criteriaId} className="border-b border-slate-900">
              <td className="py-2 pr-4">
                {/* Entries link into the agent detail view (/agents/:criteriaId). */}
                <Link
                  to={`/agents/${encodeURIComponent(a.criteriaId)}`}
                  className="text-sky-400 hover:underline"
                >
                  {a.name}
                </Link>
              </td>
              <td className="py-2 pr-4 text-slate-400">{a.labels.hostname ?? ''}</td>
              <td className={`py-2 pr-4 ${a.status === 'online' ? 'text-emerald-400' : 'text-slate-500'}`}>{a.status}</td>
              <td className="py-2 pr-4 text-slate-400">
                {a.lastSeenAt ? new Date(a.lastSeenAt).toLocaleString() : ''}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
