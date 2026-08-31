import { FormEvent, useState } from 'react';
import { Outlet, NavLink } from 'react-router-dom';
import { clearAuthToken, getAuthToken, setAuthToken } from './authToken';

const navClass = ({ isActive }: { isActive: boolean }) =>
  `px-3 py-2 rounded ${isActive ? 'bg-slate-800 text-white' : 'text-slate-400 hover:text-white'}`;

export function App() {
  const [token, setToken] = useState(() => getAuthToken());
  const [value, setValue] = useState(token);

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    const next = value.trim();
    if (!next) return;
    setAuthToken(next);
    setToken(next);
  };

  if (!token) {
    return (
      <div className="h-full flex items-center justify-center bg-slate-950 text-slate-100">
        <form onSubmit={onSubmit} className="w-full max-w-md bg-slate-900 border border-slate-800 rounded-lg p-6">
          <h1 className="text-xl font-semibold mb-3">Parapet Login</h1>
          <p className="text-sm text-slate-400 mb-4">
            Enter an agent token to authenticate Castle API and stream access.
          </p>
          <input
            value={value}
            onChange={(e) => setValue(e.target.value)}
            className="w-full bg-slate-950 border border-slate-700 rounded px-3 py-2 font-mono text-sm"
            placeholder="Agent token"
          />
          <button type="submit" className="mt-4 px-4 py-2 rounded bg-sky-600 hover:bg-sky-500 text-white">
            Continue
          </button>
        </form>
      </div>
    );
  }

  return (
    <div className="flex h-full">
      <aside className="w-56 border-r border-slate-800 p-4 flex flex-col gap-1">
        <h1 className="text-lg font-semibold mb-4">Parapet</h1>
        <NavLink to="/runs" className={navClass}>Runs</NavLink>
        <NavLink to="/agents" className={navClass}>Agents</NavLink>
        <button
          onClick={() => {
            clearAuthToken();
            setToken('');
            setValue('');
          }}
          className="mt-6 text-left px-3 py-2 rounded text-slate-400 hover:text-white hover:bg-slate-900"
        >
          Log out
        </button>
      </aside>
      <main className="flex-1 overflow-auto p-6">
        <Outlet />
      </main>
    </div>
  );
}
