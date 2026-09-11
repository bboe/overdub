#ifndef OVERDUB_AUDIO_H
#define OVERDUB_AUDIO_H

#include <stddef.h>

int audio_init(const unsigned char *pcm, size_t len, int rate, int channels);
int audio_play(void);
void audio_close(void);

#endif
