import type { Digest, FileChange } from './types';

export function fileLabel(f: FileChange): string {
  const mark = f.edited ? '*' : ' ';
  return `${mark} ${f.path}  +${f.added} -${f.removed}`;
}

export function hasFiles(d: Digest): boolean {
  return !!d.files && d.files.length > 0;
}
