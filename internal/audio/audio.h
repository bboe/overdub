#ifndef OVERDUB_AUDIO_H
#define OVERDUB_AUDIO_H

#include <stddef.h>

// The engine is built once and held. pcm is signed 16-bit little-endian, and
// belongs to the caller for the life of the player: the buffer queue keeps the
// pointer rather than a copy.
int audio_init(const unsigned char *pcm, size_t len, int rate, int channels);
int audio_play(void);
void audio_close(void);

#endif
