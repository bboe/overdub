#ifndef OVERDUB_OPUS_H
#define OVERDUB_OPUS_H

void *overdub_opus_new(int rate, int channels);
int overdub_opus_decode(void *st, const unsigned char *data, int len, short *pcm, int frames);
void overdub_opus_free(void *st);

#endif
