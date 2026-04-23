import { Outlet, NavLink } from 'react-router-dom';

const navClass = ({ isActive }: { isActive: boolean }) =>
  `px-3 py-2 rounded ${isActive ? 'bg-slate-800 text-white' : 'text-slate-400 hover:text-white'}`;

export function App() {
  return (
    <div className="flex h-full">
      <aside className="w-56 border-r border-slate-800 p-4 flex flex-col gap-1">
        <h1 className="text-lg font-semibold mb-4">Parapet</h1>
        <NavLink to="/runs" className={navClass}>Runs</NavLink>
        <NavLink to="/overseers" className={navClass}>Overseers</NavLink>
      </aside>
      <main className="flex-1 overflow-auto p-6">
        <Outlet />
      </main>
    </div>
  );
}
