export function BrandMark({ large = false }: { large?: boolean }) {
  const size = large ? 40 : 20;
  return (
    <svg xmlns="http://www.w3.org/2000/svg" className={`brand-mark${large ? ' large' : ''}`} width={size} height={size} viewBox="0 0 32 32" fill="none" aria-hidden="true" focusable="false">
      <path fill="currentColor" fillRule="evenodd" d="M 10 2 H 22 A 8 8 0 0 1 30 10 V 22 A 8 8 0 0 1 22 30 H 10 A 8 8 0 0 1 2 22 V 10 A 8 8 0 0 1 10 2 Z M 12.3500 22.4000 L 19.7500 22.4000 A 2.7500 2.7500 0 0 0 22.4423 20.2106 A 3.4500 3.4500 0 0 0 20.0612 13.7914 A 4.3500 4.3500 0 0 0 11.7963 14.2313 A 3.2000 3.2000 0 0 0 9.6376 20.1033 A 2.7500 2.7500 0 0 0 12.3500 22.4000 Z" />
    </svg>
  );
}
