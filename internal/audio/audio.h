#ifndef OVERDUB_AUDIO_H
#define OVERDUB_AUDIO_H

#include <stddef.h>

int audio_open(int rate, int channels);
int audio_write(const unsigned char *pcm, size_t len);
int audio_start(void);
void audio_close(void);

#endif
