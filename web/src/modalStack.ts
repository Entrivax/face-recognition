// Escape-stack registry: Escape closes the topmost open modal only. Each
// open modal pushes a close callback; App installs the single document
// keydown handler that pops the top entry.

type CloseFn = () => void;

const stack: CloseFn[] = [];

export function pushEscape(fn: CloseFn): void {
	stack.push(fn);
}

export function removeEscape(fn: CloseFn): void {
	const i = stack.lastIndexOf(fn);
	if (i >= 0) stack.splice(i, 1);
}

// Runs the topmost modal's close callback. Returns false when no modal is
// open (the keydown handler ignores the key then).
export function handleEscape(): boolean {
	const top = stack[stack.length - 1];
	if (!top) return false;
	top();
	return true;
}
