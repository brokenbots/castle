import { Fragment } from 'react';
import { Link } from 'react-router-dom';

export interface Crumb {
  label: string;
  // Omit for the current page, which renders as the terminal crumb.
  to?: string;
}

interface BreadcrumbsProps {
  items: Crumb[];
}

// Breadcrumb trail for deep pages (run detail, agent detail): ancestor links
// followed by the current location. Ancestors navigate; the terminal crumb is
// marked aria-current="page" and is not a link.
export function Breadcrumbs({ items }: BreadcrumbsProps) {
  return (
    <nav data-testid="breadcrumbs" aria-label="Breadcrumb">
      <ol className="flex flex-wrap items-center gap-1 text-body text-ink-muted">
        {items.map((item, index) => {
          const current = index === items.length - 1;
          const link = item.to && !current;
          return (
            <Fragment key={`${index}:${item.label}`}>
              {index > 0 && (
                <li aria-hidden="true" className="text-ink-faint">
                  /
                </li>
              )}
              <li>
                {link ? (
                  <Link to={item.to!} className="hover:text-ink hover:underline">
                    {item.label}
                  </Link>
                ) : (
                  <span aria-current={current ? 'page' : undefined} className={current ? 'text-ink' : undefined}>
                    {item.label}
                  </span>
                )}
              </li>
            </Fragment>
          );
        })}
      </ol>
    </nav>
  );
}