import { castleRunDataSource } from './castleDataSource';
import { setRunDataSource } from './dataSource';

// Package defaults: when imported (the package index does), the seam is
// backed by the castle Connect client, preserving parapet's behavior with
// zero host wiring. The standalone viewer imports the package pieces it
// needs directly and calls setRunDataSource(localRunDataSource) instead.
setRunDataSource(castleRunDataSource);