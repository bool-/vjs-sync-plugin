// Loads a source into a media element. HLS plays natively where the
// browser can (Safari, iOS); elsewhere hls.js is fetched on first use, so
// MP4-only pages never download it.

const HLS_TYPES = ['application/x-mpegurl', 'application/vnd.apple.mpegurl'];

export const DEFAULT_HLS_URL = 'https://cdn.jsdelivr.net/npm/hls.js@1/dist/hls.mjs';

/**
 * @param  {string|{src: string, type?: string}} source
 * @return {{src: string, type: string}}
 */
export const normalizeSource = (source) =>
  typeof source === 'string' ? {src: source, type: ''} : {src: source.src, type: source.type || ''};

/**
 * @param  {{src: string, type: string}} source
 * @return {boolean}
 */
export const isHls = ({src, type}) =>
  HLS_TYPES.includes(type.toLowerCase()) || /\.m3u8($|\?)/i.test(src);

/**
 * Point a media element at a source.
 *
 * @param  {HTMLMediaElement} media
 * @param  {string|{src: string, type?: string}} source
 * @param  {string} [hlsUrl] Where to import hls.js from when it is needed.
 * @return {Promise<Function>} Resolves to a function that releases the source.
 */
export const loadSource = async (media, source, hlsUrl = DEFAULT_HLS_URL) => {
  const normalized = normalizeSource(source);

  if (!isHls(normalized) || media.canPlayType('application/vnd.apple.mpegurl')) {
    media.src = normalized.src;
    return () => media.removeAttribute('src');
  }

  const {default: Hls} = await import(hlsUrl);

  if (!Hls.isSupported()) {
    media.src = normalized.src;
    return () => media.removeAttribute('src');
  }

  const hls = new Hls();

  hls.loadSource(normalized.src);
  hls.attachMedia(media);
  return () => hls.destroy();
};
