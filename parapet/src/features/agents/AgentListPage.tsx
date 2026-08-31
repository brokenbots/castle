import { useListAgentsQuery } from '../../api/castleApi';

export function AgentListPage() {
  const { data, isLoading, error } = useListAgentsQuery();
  if (isLoading) return <p>Loading…</p>;
  if (error) return <p className="text-rose-400">Failed to load.</p>;
  return (
    <div>
      <h2 className="text-2xl font-semibold mb-4">Agents</h2>
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
              <td className="py-2 pr-4">{a.name}</td>
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
